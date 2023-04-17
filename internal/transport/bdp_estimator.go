/*
 *
 * Copyright 2017 gRPC authors.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 *
 */

package transport

import (
	"sync"
	"time"
)

const (
	// bdpLimit is the maximum value the flow control windows will be increased
	// to.  TCP typically limits this to 4MB(这应该是tcp接收缓存最大值), but some systems go up to 16MB.
	// Since this is only a limit, it is safe to make it optimistic.
	bdpLimit = (1 << 20) * 16
	// alpha is a constant factor used to keep a moving average
	// of RTTs.
	alpha = 0.9
	// If the current bdp sample is greater than or equal to
	// our beta * our estimated bdp and the current bandwidth
	// sample is the maximum bandwidth observed so far, we
	// increase our bbp estimate by a factor of gamma.
	beta = 0.66
	// To put our bdp to be smaller than or equal to twice the real BDP,
	// we should multiply our current sample with 4/3, however to round things out
	// we use 2 as the multiplication factor.
	gamma = 2
)

// Adding arbitrary data to ping so that its ack can be identified.
// Easter-egg: what does the ping message say?
// 数字bdpping 在字母表中的顺序
var bdpPing = &ping{data: [8]byte{2, 4, 16, 16, 9, 14, 7, 7}}

type bdpEstimator struct {
	// sentAt is the time when the ping was sent.
	sentAt time.Time

	mu sync.Mutex
	// bdp is the current bdp estimate.
	bdp uint32
	// sample is the number of bytes received in one measurement cycle.
	sample uint32
	// bwMax is the maximum bandwidth noted so far (bytes/sec).
	bwMax float64
	// bool to keep track of the beginning of a new measurement cycle.
	isSent bool
	// Callback to update the window sizes.
	updateFlowControl func(n uint32)
	// sampleCount is the number of samples taken so far.
	sampleCount uint64
	// round trip time (seconds)
	rtt float64
}

// timesnap registers the time bdp ping was sent out so that
// network rtt can be calculated when its ack is received.
// It is called (by controller) when the bdpPing is
// being written on the wire.
func (b *bdpEstimator) timesnap(d [8]byte) {
	// 检查是否是bdpping
	if bdpPing.data != d {
		return
	}
	b.sentAt = time.Now()
}

// add adds bytes to the current sample for calculating bdp.
// It returns true only if a ping must be sent. This can be used
// by the caller (handleData) to make decision about batching
// a window update with it.
// 只有当需要发送bdping的时候才返回true
func (b *bdpEstimator) add(n uint32) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	// 已达到上限 无需处理
	if b.bdp == bdpLimit {
		return false
	}
	// 如果未发送bdpping则记录数据，返回发送标识
	if !b.isSent {
		b.isSent = true
		b.sample = n
		b.sentAt = time.Time{}
		b.sampleCount++
		return true
	}
	// 已发送则直接追加数据
	b.sample += n
	return false
}

// calculate is called when an ack for a bdp ping is received.
// Here we calculate the current bdp and bandwidth sample and
// decide if the flow control windows should go up.
func (b *bdpEstimator) calculate(d [8]byte) {
	// Check if the ping acked for was the bdp ping.
	// 检查是否是bdpping
	if bdpPing.data != d {
		return
	}
	b.mu.Lock()
	// rtt 采样耗时 单位为秒
	rttSample := time.Since(b.sentAt).Seconds()
	if b.sampleCount < 10 {
		// Bootstrap rtt with an average of first 10 rtt samples.
		// 前10次采样使用平均值
		b.rtt += (rttSample - b.rtt) / float64(b.sampleCount)
	} else {
		// Heed to the recent past more.
		// 后续采用基于固定比例使之更接近当前值 官方解释 https://grpc.io/blog/grpc-go-perf-improvements/
		b.rtt += (rttSample - b.rtt) * float64(alpha)
	}
	b.isSent = false
	// The number of bytes accumulated so far in the sample is smaller
	// than or equal to 1.5 times the real BDP on a saturated饱和的 connection.
	bwCurrent := float64(b.sample) / (b.rtt * float64(1.5))

	// 比之前计算的带宽还要大则更新
	if bwCurrent > b.bwMax {
		b.bwMax = bwCurrent
	}
	// If the current sample (which is smaller than or equal to the 1.5 times the real BDP) is
	// greater than or equal to 2/3rd our perceived bdp AND this is the maximum bandwidth seen so far, we
	// should update our perception of the network BDP.
	// 如果当前样本bdp已经达到默认bdp2/3以上并且没有触达上限 则bdp扩大至样本2倍
	if float64(b.sample) >= beta*float64(b.bdp) && bwCurrent == b.bwMax && b.bdp != bdpLimit {
		sampleFloat := float64(b.sample)
		b.bdp = uint32(gamma * sampleFloat)
		if b.bdp > bdpLimit {
			b.bdp = bdpLimit
		}
		bdp := b.bdp
		b.mu.Unlock()
		b.updateFlowControl(bdp)
		return
	}
	b.mu.Unlock()
}
