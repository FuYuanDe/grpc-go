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

package grpc

import (
	"errors"
	"fmt"

	"google.golang.org/grpc/balancer"
	"google.golang.org/grpc/connectivity"
)

// PickFirstBalancerName is the name of the pick_first balancer.
const PickFirstBalancerName = "pick_first"

func newPickfirstBuilder() balancer.Builder {
	return &pickfirstBuilder{}
}

type pickfirstBuilder struct{}

func (*pickfirstBuilder) Build(cc balancer.ClientConn, opt balancer.BuildOptions) balancer.Balancer {
	return &pickfirstBalancer{cc: cc}
}

func (*pickfirstBuilder) Name() string {
	return PickFirstBalancerName
}

type pickfirstBalancer struct {
	state   connectivity.State
	cc      balancer.ClientConn
	subConn balancer.SubConn
}

func (b *pickfirstBalancer) ResolverError(err error) {
	if logger.V(2) {
		logger.Infof("pickfirstBalancer: ResolverError called with error: %v", err)
	}
	if b.subConn == nil {
		b.state = connectivity.TransientFailure
	}

	if b.state != connectivity.TransientFailure {
		// The picker will not change since the balancer does not currently
		// report an error.
		return
	}
	b.cc.UpdateState(balancer.State{
		ConnectivityState: connectivity.TransientFailure,
		Picker:            &picker{err: fmt.Errorf("name resolver error: %v", err)},
	})
}

// 更新cs状态
// 服务发现更新后端地址列表
func (b *pickfirstBalancer) UpdateClientConnState(state balancer.ClientConnState) error {

	if len(state.ResolverState.Addresses) == 0 {
		// 地址列表为空则当做解析失败,同时清空旧的链接
		// The resolver reported an empty address list. Treat it like an error by
		// calling b.ResolverError.
		if b.subConn != nil {
			// Remove the old subConn. All addresses were removed, so it is no longer
			// valid.
			b.cc.RemoveSubConn(b.subConn)
			b.subConn = nil
		}
		b.ResolverError(errors.New("produced zero addresses"))
		return balancer.ErrBadResolverState
	}

	// 已经创建过链接则更新
	if b.subConn != nil {
		b.cc.UpdateAddresses(b.subConn, state.ResolverState.Addresses)
		return nil
	}
	// 为创建链接则新建链接

	// 这一步有创建底层链接吗？貌似没有，下面更新状态为idle，这一段难道仅仅是创建结构体
	subConn, err := b.cc.NewSubConn(state.ResolverState.Addresses, balancer.NewSubConnOptions{})
	if err != nil {
		if logger.V(2) {
			logger.Errorf("pickfirstBalancer: failed to NewSubConn: %v", err)
		}
		b.state = connectivity.TransientFailure
		b.cc.UpdateState(balancer.State{
			ConnectivityState: connectivity.TransientFailure,
			Picker:            &picker{err: fmt.Errorf("error creating connection: %v", err)},
		})
		return balancer.ErrBadResolverState
	}
	b.subConn = subConn
	b.state = connectivity.Idle
	b.cc.UpdateState(balancer.State{
		ConnectivityState: connectivity.Connecting,
		Picker:            &picker{err: balancer.ErrNoSubConnAvailable},
	})
	// 这里才是真正创建连接吧？
	b.subConn.Connect()
	return nil
}

// 更新子链接状态 如idle、连接中、ready等等
func (b *pickfirstBalancer) UpdateSubConnState(subConn balancer.SubConn, state balancer.SubConnState) {
	if logger.V(2) {
		logger.Infof("pickfirstBalancer: UpdateSubConnState: %p, %v", subConn, state)
	}
	// pickerBalancer的subconn 应该一致。如果subconn挂掉了，在哪里更新它
	if b.subConn != subConn {
		if logger.V(2) {
			logger.Infof("pickfirstBalancer: ignored state change because subConn is not recognized")
		}
		return
	}
	b.state = state.ConnectivityState
	if state.ConnectivityState == connectivity.Shutdown {
		// 链接断开了，啥场景下会出现这种情况？客户端主动断链？服务端主动断链？
		// 服务发现地址列表为空，此时会释放旧链接
		b.subConn = nil
		return
	}

	switch state.ConnectivityState {
	// 链接OK，可以pick返回
	case connectivity.Ready:
		// 链接OK了，将链接塞到picker result里
		b.cc.UpdateState(balancer.State{
			ConnectivityState: state.ConnectivityState,
			Picker:            &picker{result: balancer.PickResult{SubConn: subConn}},
		})
	case connectivity.Connecting:
		// 正在连接，pick不可用，返回not available,正常情况下，需要继续等待
		b.cc.UpdateState(balancer.State{
			ConnectivityState: state.ConnectivityState,
			Picker:            &picker{err: balancer.ErrNoSubConnAvailable},
		})
	case connectivity.Idle:
		// 链接闲置状态，设置idlePicker, 当pick的时候会再去链接
		b.cc.UpdateState(balancer.State{
			ConnectivityState: state.ConnectivityState,
			Picker:            &idlePicker{subConn: subConn},
		})
	case connectivity.TransientFailure:
		// 失败，返回链接异常
		b.cc.UpdateState(balancer.State{
			ConnectivityState: state.ConnectivityState,
			Picker:            &picker{err: state.ConnectionError},
		})
	}
}

func (b *pickfirstBalancer) Close() {
}

func (b *pickfirstBalancer) ExitIdle() {
	if b.subConn != nil && b.state == connectivity.Idle {
		b.subConn.Connect()
	}
}

type picker struct {
	result balancer.PickResult
	err    error
}

func (p *picker) Pick(balancer.PickInfo) (balancer.PickResult, error) {
	return p.result, p.err
}

// idlePicker is used when the SubConn is IDLE and kicks the SubConn into
// CONNECTING when Pick is called.
type idlePicker struct {
	subConn balancer.SubConn
}

func (i *idlePicker) Pick(balancer.PickInfo) (balancer.PickResult, error) {
	i.subConn.Connect()
	return balancer.PickResult{}, balancer.ErrNoSubConnAvailable
}

func init() {
	balancer.Register(newPickfirstBuilder())
}
