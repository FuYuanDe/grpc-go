/*
 *
 * Copyright 2014 gRPC authors.
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
	"bytes"
	"errors"
	"fmt"
	"runtime"
	"strconv"
	"sync"
	"sync/atomic"

	"golang.org/x/net/http2"
	"golang.org/x/net/http2/hpack"
	"google.golang.org/grpc/internal/grpcutil"
	"google.golang.org/grpc/status"
)

var updateHeaderTblSize = func(e *hpack.Encoder, v uint32) {
	e.SetMaxDynamicTableSizeLimit(v)
}

type itemNode struct {
	it   interface{}
	next *itemNode
}

// FIFO队列
type itemList struct {
	head *itemNode
	tail *itemNode
}

func (il *itemList) enqueue(i interface{}) {
	n := &itemNode{it: i}
	if il.tail == nil {
		il.head, il.tail = n, n
		return
	}
	il.tail.next = n
	il.tail = n
}

// peek returns the first item in the list without removing it from the
// list.
func (il *itemList) peek() interface{} {
	return il.head.it
}

// 出队
func (il *itemList) dequeue() interface{} {
	if il.head == nil {
		return nil
	}
	i := il.head.it
	il.head = il.head.next
	if il.head == nil {
		il.tail = nil
	}
	return i
}

func (il *itemList) dequeueAll() *itemNode {
	h := il.head
	il.head, il.tail = nil, nil
	return h
}

func (il *itemList) isEmpty() bool {
	return il.head == nil
}

// The following defines various control items which could flow through
// the control buffer of transport. They represent different aspects of
// control tasks, e.g., flow control, settings, streaming resetting, etc.

// maxQueuedTransportResponseFrames is the most queued "transport response"
// frames we will buffer before preventing new reads from occurring on the
// transport.  These are control frames sent in response to client requests,
// such as RST_STREAM due to bad headers or settings acks.
const maxQueuedTransportResponseFrames = 50

type cbItem interface {
	isTransportResponseFrame() bool
}

// registerStream is used to register an incoming stream with loopy writer.
type registerStream struct {
	streamID uint32
	wq       *writeQuota
}

func (*registerStream) isTransportResponseFrame() bool { return false }

// headerFrame is also used to register stream on the client-side.
type headerFrame struct {
	streamID   uint32
	hf         []hpack.HeaderField
	endStream  bool               // Valid on server side.
	initStream func(uint32) error // Used only on the client side.
	onWrite    func()
	wq         *writeQuota    // write quota for the stream created.
	cleanup    *cleanupStream // Valid on the server side.
	onOrphaned func(error)    // Valid on client-side
}

func (h *headerFrame) isTransportResponseFrame() bool {
	return h.cleanup != nil && h.cleanup.rst // Results in a RST_STREAM
}

type cleanupStream struct {
	streamID uint32
	rst      bool
	rstCode  http2.ErrCode
	onWrite  func()
}

func (c *cleanupStream) isTransportResponseFrame() bool { return c.rst } // Results in a RST_STREAM

type earlyAbortStream struct {
	httpStatus     uint32
	streamID       uint32
	contentSubtype string
	status         *status.Status
	rst            bool
}

func (*earlyAbortStream) isTransportResponseFrame() bool { return false }

type dataFrame struct {
	streamID  uint32
	endStream bool
	h         []byte
	d         []byte
	// onEachWrite is called every time
	// a part of d is written out.
	onEachWrite func()
}

func (*dataFrame) isTransportResponseFrame() bool { return false }

type incomingWindowUpdate struct {
	streamID  uint32
	increment uint32
}

func (*incomingWindowUpdate) isTransportResponseFrame() bool { return false }

type outgoingWindowUpdate struct {
	streamID  uint32
	increment uint32
}

func (*outgoingWindowUpdate) isTransportResponseFrame() bool {
	return false // window updates are throttled by thresholds
}

type incomingSettings struct {
	ss []http2.Setting
}

func (*incomingSettings) isTransportResponseFrame() bool { return true } // Results in a settings ACK

type outgoingSettings struct {
	ss []http2.Setting
}

func (*outgoingSettings) isTransportResponseFrame() bool { return false }

type incomingGoAway struct {
}

func (*incomingGoAway) isTransportResponseFrame() bool { return false }

type goAway struct {
	code      http2.ErrCode
	debugData []byte
	headsUp   bool
	closeConn error // if set, loopyWriter will exit, resulting in conn closure
}

func (*goAway) isTransportResponseFrame() bool { return false }

type ping struct {
	ack  bool
	data [8]byte
}

func (*ping) isTransportResponseFrame() bool { return true }

type outFlowControlSizeRequest struct {
	resp chan uint32
}

func (*outFlowControlSizeRequest) isTransportResponseFrame() bool { return false }

// closeConnection is an instruction to tell the loopy writer to flush the
// framer and exit, which will cause the transport's connection to be closed
// (by the client or server).  The transport itself will close after the reader
// encounters the EOF caused by the connection closure.
type closeConnection struct{}

func (closeConnection) isTransportResponseFrame() bool { return false }

type outStreamState int

const (
	active outStreamState = iota
	empty
	waitingOnStreamQuota
)

type outStream struct {
	id               uint32         // 流ID
	state            outStreamState // 流状态
	itl              *itemList      // fifo 消息列表
	bytesOutStanding int            // 发送出去的字节数 用于流控
	wq               *writeQuota

	next *outStream
	prev *outStream
}

func (s *outStream) deleteSelf() {
	if s.prev != nil {
		s.prev.next = s.next
	}
	if s.next != nil {
		s.next.prev = s.prev
	}
	s.next, s.prev = nil, nil
}

type outStreamList struct {
	// Following are sentinel objects that mark the
	// beginning and end of the list. They do not
	// contain any item lists. All valid objects are
	// inserted in between them.
	// This is needed so that an outStream object can
	// deleteSelf() in O(1) time without knowing which
	// list it belongs to.
	head *outStream
	tail *outStream
}

func newOutStreamList() *outStreamList {
	head, tail := new(outStream), new(outStream)
	head.next = tail
	tail.prev = head
	return &outStreamList{
		head: head,
		tail: tail,
	}
}

func (l *outStreamList) enqueue(s *outStream) {
	e := l.tail.prev
	e.next = s
	s.prev = e
	s.next = l.tail
	l.tail.prev = s
}

// remove from the beginning of the list.
func (l *outStreamList) dequeue() *outStream {
	b := l.head.next
	if b == l.tail {
		return nil
	}
	b.deleteSelf()
	return b
}

// controlBuffer is a way to pass information to loopy.
// Information is passed as specific struct types called control frames.
// A control frame not only represents data, messages or headers to be sent out
// but can also be used to instruct loopy to update its internal state.
// It shouldn't be confused with an HTTP2 frame, although some of the control frames
// like dataFrame and headerFrame do go out on wire as HTTP2 frames.
type controlBuffer struct {
	ch              chan struct{}
	done            <-chan struct{}
	mu              sync.Mutex
	consumerWaiting bool
	list            *itemList // 消息列表
	err             error

	// transportResponseFrames counts the number of queued items that represent
	// the response of an action initiated by the peer.  trfChan is created
	// when transportResponseFrames >= maxQueuedTransportResponseFrames and is
	// closed and nilled when transportResponseFrames drops below the
	// threshold.  Both fields are protected by mu.
	transportResponseFrames int
	trfChan                 atomic.Value // chan struct{}
}

func newControlBuffer(done <-chan struct{}) *controlBuffer {
	return &controlBuffer{
		ch:   make(chan struct{}, 1),
		list: &itemList{},
		done: done,
	}
}

// throttle blocks if there are too many incomingSettings/cleanupStreams in the
// controlbuf.
func (c *controlBuffer) throttle() {
	ch, _ := c.trfChan.Load().(chan struct{})
	if ch != nil {
		select {
		case <-ch:
		case <-c.done:
		}
	}
}

func (c *controlBuffer) put(it cbItem) error {
	_, err := c.executeAndPut(nil, it)
	return err
}

// 返回的bool用来做啥？？？
func (c *controlBuffer) executeAndPut(f func(it interface{}) bool, it cbItem) (bool, error) {
	var wakeUp bool
	c.mu.Lock()
	if c.err != nil {
		c.mu.Unlock()
		return false, c.err
	}
	if f != nil {
		if !f(it) { // f wasn't successful
			c.mu.Unlock()
			return false, nil
		}
	}
	if c.consumerWaiting {
		wakeUp = true
		c.consumerWaiting = false
	}
	// 加到队尾
	c.list.enqueue(it)
	if it.isTransportResponseFrame() {
		c.transportResponseFrames++
		if c.transportResponseFrames == maxQueuedTransportResponseFrames {
			// We are adding the frame that puts us over the threshold; create
			// a throttling channel.
			c.trfChan.Store(make(chan struct{}))
		}
	}
	c.mu.Unlock()
	if wakeUp {
		select {
		case c.ch <- struct{}{}:
		default:
		}
	}
	return true, nil
}

// Note argument f should never be nil.
func (c *controlBuffer) execute(f func(it interface{}) bool, it interface{}) (bool, error) {
	c.mu.Lock()
	if c.err != nil {
		c.mu.Unlock()
		return false, c.err
	}
	if !f(it) { // f wasn't successful
		c.mu.Unlock()
		return false, nil
	}
	c.mu.Unlock()
	return true, nil
}

// 获取一个消息
func (c *controlBuffer) get(block bool) (interface{}, error) {
	for {
		c.mu.Lock()
		// 检查是否有异常 啥情况下会出现异常
		if c.err != nil {
			c.mu.Unlock()
			return nil, c.err
		}
		if !c.list.isEmpty() {
			// 取队列头消息
			h := c.list.dequeue().(cbItem)
			if h.isTransportResponseFrame() {
				if c.transportResponseFrames == maxQueuedTransportResponseFrames {
					// We are removing the frame that put us over the
					// threshold; close and clear the throttling channel.
					ch := c.trfChan.Load().(chan struct{})
					close(ch)
					c.trfChan.Store((chan struct{})(nil))
				}
				c.transportResponseFrames--
			}
			c.mu.Unlock()
			return h, nil
		}
		// 队列为空 非阻塞情况下直接返回 阻塞情况下等待
		if !block {
			c.mu.Unlock()
			return nil, nil
		}
		// 设置waiting标志位
		c.consumerWaiting = true
		c.mu.Unlock()
		// 等待消息
		select {
		case <-c.ch:
		case <-c.done:
			return nil, errors.New("transport closed by client")
		}
	}
}

func (c *controlBuffer) finish() {
	c.mu.Lock()
	if c.err != nil {
		c.mu.Unlock()
		return
	}
	c.err = ErrConnClosing
	// There may be headers for streams in the control buffer.
	// These streams need to be cleaned out since the transport
	// is still not aware of these yet.
	for head := c.list.dequeueAll(); head != nil; head = head.next {
		hdr, ok := head.it.(*headerFrame)
		if !ok {
			continue
		}
		if hdr.onOrphaned != nil { // It will be nil on the server-side.
			hdr.onOrphaned(ErrConnClosing)
		}
	}
	// In case throttle() is currently in flight, it needs to be unblocked.
	// Otherwise, the transport may not close, since the transport is closed by
	// the reader encountering the connection error.
	ch, _ := c.trfChan.Load().(chan struct{})
	if ch != nil {
		close(ch)
	}
	c.trfChan.Store((chan struct{})(nil))
	c.mu.Unlock()
}

type side int

const (
	clientSide side = iota
	serverSide
)

// Loopy receives frames from the control buffer.
// Each frame is handled individually; most of the work done by loopy goes
// into handling data frames. Loopy maintains a queue of active streams, and each
// stream maintains a queue of data frames; as loopy receives data frames
// it gets added to the queue of the relevant stream.
// Loopy goes over this list of active streams by processing one node every iteration,
// thereby closely resemebling to a round-robin scheduling over all streams. While
// processing a stream, loopy writes out data bytes from this stream capped by the min
// of http2MaxFrameLen, connection-level flow control and stream-level flow control.
// loopy 从control buffer 接收帧，维护一个 active stream列表。每个stream维护一个data frame队列
// loopy每次循环处理一个active stream
type loopyWriter struct {
	side      side
	cbuf      *controlBuffer
	sendQuota uint32 // 用于配合对端connection flow control, 表示 conn window size
	oiws      uint32 // outbound initial window size. // 用于配合对端stream flow control, 表示对端stream窗口大小

	// estdStreams is map of all established streams that are not cleaned-up yet.
	// On client-side, this is all streams whose headers were sent out.
	// On server-side, this is all streams whose headers were received.
	estdStreams map[uint32]*outStream // Established streams.
	// activeStreams is a linked-list of all streams that have data to send and some
	// stream-level flow control quota.
	// Each of these streams internally have a list of data items(and perhaps trailers
	// on the server-side) to be sent out.
	activeStreams *outStreamList
	framer        *framer
	hBuf          *bytes.Buffer  // The buffer for HPACK encoding.
	hEnc          *hpack.Encoder // HPACK encoder.
	bdpEst        *bdpEstimator  // dbp 估算器 用于动态窗口更新
	draining      bool           // 清空标识

	// Side-specific handlers
	ssGoAwayHandler func(*goAway) (bool, error)
}

func newLoopyWriter(s side, fr *framer, cbuf *controlBuffer, bdpEst *bdpEstimator) *loopyWriter {
	var buf bytes.Buffer
	l := &loopyWriter{
		side:          s,
		cbuf:          cbuf,
		sendQuota:     defaultWindowSize, // 用于配合对端流量控制 conn flow window 默认值65535
		oiws:          defaultWindowSize, // output stream flow window
		estdStreams:   make(map[uint32]*outStream),
		activeStreams: newOutStreamList(),
		framer:        fr,
		hBuf:          &buf,
		hEnc:          hpack.NewEncoder(&buf),
		bdpEst:        bdpEst,
	}
	return l
}

const minBatchSize = 1000

// run should be run in a separate goroutine.
// It reads control frames from controlBuf and processes them by:
// 1. Updating loopy's internal state, or/and
// 2. Writing out HTTP2 frames on the wire.
//
// Loopy keeps all active streams with data to send in a linked-list.
// All streams in the activeStreams linked-list must have both:
// 1. Data to send, and
// 2. Stream level flow control quota available.
//
// In each iteration of run loop, other than processing the incoming control
// frame, loopy calls processData, which processes one node from the activeStreams linked-list.
// This results in writing of HTTP2 frames into an underlying write buffer.
// When there's no more control frames to read from controlBuf, loopy flushes the write buffer.
// As an optimization, to increase the batch size for each flush, loopy yields the processor, once
// if the batch size is too low to give stream goroutines a chance to fill it up.
//
// run 应该在独立协程中运行，它从controlBuf读取消息并处理，包括更新loopy自身状态或者发送http2 报文
//
// loopy 将所有需要发送数据的stream放到一个active链表中，active链表中的每个stream必须满足两个条件，1，有数据发送。2，不受stream 流控
//
// 在运行循环的每次迭代中，除了处理传入的控制帧之外，循环调用processData，处理activeStreams链表中的一个节点。
// 这将导致HTTP2帧写入底层写缓冲区。当没有更多的控制帧可以从controlBuf读取时，循环刷新写入缓冲区。
// 作为一种优化，为了增加每次刷新的批量大小，loopy会短暂交出处理器权限，使得stream有机会填充发送缓冲区
func (l *loopyWriter) run() (err error) {
	// Always flush the writer before exiting in case there are pending frames
	// to be sent.
	// 退出之前清空下发送缓存
	defer l.framer.writer.Flush()
	for {
		// 阻塞读消息(setting、header、data、ping、goaway frame)
		it, err := l.cbuf.get(true)
		if err != nil {
			return err
		}
		// 消息处理
		if err = l.handle(it); err != nil {
			return err
		}
		// 发送data frame
		if _, err = l.processData(); err != nil {
			return err
		}
		// 批量写
		gosched := true
	hasdata:
		for {
			// 非阻塞读消息
			it, err := l.cbuf.get(false)
			if err != nil {
				return err
			}
			if it != nil {
				// 存在消息则循环处理
				if err = l.handle(it); err != nil {
					return err
				}
				if _, err = l.processData(); err != nil {
					return err
				}
				continue hasdata
			}

			isEmpty, err := l.processData()
			if err != nil {
				return err
			}
			if !isEmpty { // 还有data待处理 则继续
				continue hasdata
			}
			if gosched {
				gosched = false
				// 批量写
				if l.framer.writer.offset < minBatchSize {
					runtime.Gosched()
					continue hasdata
				}
			}
			l.framer.writer.Flush()
			break hasdata
		}
	}
}

func (l *loopyWriter) outgoingWindowUpdateHandler(w *outgoingWindowUpdate) error {
	return l.framer.fr.WriteWindowUpdate(w.streamID, w.increment)
}

// 收到窗口变更报文
func (l *loopyWriter) incomingWindowUpdateHandler(w *incomingWindowUpdate) error {
	// Otherwise update the quota.
	// 作用于整个链路
	if w.streamID == 0 {
		l.sendQuota += w.increment
		return nil
	}
	// Find the stream and update it.
	// 作用于单个stream
	if str, ok := l.estdStreams[w.streamID]; ok {
		str.bytesOutStanding -= int(w.increment)
		// 如果该stream正在等待发送额度则激活它，并将只添加到活跃队列尾部
		if strQuota := int(l.oiws) - str.bytesOutStanding; strQuota > 0 && str.state == waitingOnStreamQuota {
			str.state = active
			l.activeStreams.enqueue(str)
			return nil
		}
	}
	return nil
}

func (l *loopyWriter) outgoingSettingsHandler(s *outgoingSettings) error {
	return l.framer.fr.WriteSettings(s.ss...)
}

func (l *loopyWriter) incomingSettingsHandler(s *incomingSettings) error {
	if err := l.applySettings(s.ss); err != nil {
		return err
	}
	return l.framer.fr.WriteSettingsAck()
}

func (l *loopyWriter) registerStreamHandler(h *registerStream) error {
	str := &outStream{
		id:    h.streamID,
		state: empty,
		itl:   &itemList{},
		wq:    h.wq,
	}
	l.estdStreams[h.streamID] = str
	return nil
}

func (l *loopyWriter) headerHandler(h *headerFrame) error {
	if l.side == serverSide { // 服务侧
		str, ok := l.estdStreams[h.streamID]
		if !ok {
			if logger.V(logLevel) {
				logger.Warningf("transport: loopy doesn't recognize the stream: %d", h.streamID)
			}
			return nil
		}
		// Case 1.A: Server is responding back with headers.
		if !h.endStream {
			// server端响应header报文
			return l.writeHeader(h.streamID, h.endStream, h.hf, h.onWrite)
		}
		// else:  Case 1.B: Server wants to close stream.

		if str.state != empty { // either active or waiting on stream quota.
			// add it str's list of items.
			// 可能是trailer 部分
			str.itl.enqueue(h)
			return nil
		}

		// empty状态下才走到这里，即发送end-stream 报文
		if err := l.writeHeader(h.streamID, h.endStream, h.hf, h.onWrite); err != nil {
			return err
		}
		return l.cleanupStreamHandler(h.cleanup)
	}
	// Case 2: Client wants to originate stream. 发起
	str := &outStream{
		id:    h.streamID,
		state: empty,
		itl:   &itemList{},
		wq:    h.wq,
	}
	// client 初始化stream
	return l.originateStream(str, h)
}

func (l *loopyWriter) originateStream(str *outStream, hdr *headerFrame) error {
	// l.draining is set when handling GoAway. In which case, we want to avoid
	// creating new streams.
	if l.draining { // 链接断开
		// TODO: provide a better error with the reason we are in draining.
		hdr.onOrphaned(errStreamDrain)
		return nil
	}
	// 初始化主要是通知保活线程
	if err := hdr.initStream(str.id); err != nil {
		return err
	}
	if err := l.writeHeader(str.id, hdr.endStream, hdr.hf, hdr.onWrite); err != nil {
		return err
	}
	l.estdStreams[str.id] = str
	return nil
}

func (l *loopyWriter) writeHeader(streamID uint32, endStream bool, hf []hpack.HeaderField, onWrite func()) error {
	if onWrite != nil {
		onWrite()
	}
	l.hBuf.Reset()
	for _, f := range hf {
		if err := l.hEnc.WriteField(f); err != nil {
			if logger.V(logLevel) {
				logger.Warningf("transport: loopyWriter.writeHeader encountered error while encoding headers: %v", err)
			}
		}
	}
	var (
		err               error
		endHeaders, first bool
	)
	first = true
	for !endHeaders {
		size := l.hBuf.Len()
		if size > http2MaxFrameLen {
			size = http2MaxFrameLen
		} else {
			endHeaders = true
		}
		if first {
			first = false
			err = l.framer.fr.WriteHeaders(http2.HeadersFrameParam{
				StreamID:      streamID,
				BlockFragment: l.hBuf.Next(size),
				EndStream:     endStream,
				EndHeaders:    endHeaders,
			})
		} else {
			err = l.framer.fr.WriteContinuation(
				streamID,
				endHeaders,
				l.hBuf.Next(size),
			)
		}
		if err != nil {
			return err
		}
	}
	return nil
}

func (l *loopyWriter) preprocessData(df *dataFrame) error {
	str, ok := l.estdStreams[df.streamID]
	if !ok {
		return nil
	}
	// If we got data for a stream it means that
	// stream was originated and the headers were sent out.
	str.itl.enqueue(df)
	if str.state == empty {
		str.state = active
		l.activeStreams.enqueue(str)
	}
	return nil
}

func (l *loopyWriter) pingHandler(p *ping) error {
	if !p.ack {
		l.bdpEst.timesnap(p.data)
	}
	return l.framer.fr.WritePing(p.ack, p.data)

}

func (l *loopyWriter) outFlowControlSizeRequestHandler(o *outFlowControlSizeRequest) error {
	o.resp <- l.sendQuota
	return nil
}

func (l *loopyWriter) cleanupStreamHandler(c *cleanupStream) error {
	c.onWrite()
	if str, ok := l.estdStreams[c.streamID]; ok {
		// On the server side it could be a trailers-only response or
		// a RST_STREAM before stream initialization thus the stream might
		// not be established yet.
		delete(l.estdStreams, c.streamID)
		str.deleteSelf()
	}
	if c.rst { // If RST_STREAM needs to be sent.
		if err := l.framer.fr.WriteRSTStream(c.streamID, c.rstCode); err != nil {
			return err
		}
	}
	if l.draining && len(l.estdStreams) == 0 {
		return errors.New("finished processing active streams while in draining mode")
	}
	return nil
}

func (l *loopyWriter) earlyAbortStreamHandler(eas *earlyAbortStream) error {
	if l.side == clientSide {
		return errors.New("earlyAbortStream not handled on client")
	}
	// In case the caller forgets to set the http status, default to 200.
	if eas.httpStatus == 0 {
		eas.httpStatus = 200
	}
	headerFields := []hpack.HeaderField{
		{Name: ":status", Value: strconv.Itoa(int(eas.httpStatus))},
		{Name: "content-type", Value: grpcutil.ContentType(eas.contentSubtype)},
		{Name: "grpc-status", Value: strconv.Itoa(int(eas.status.Code()))},
		{Name: "grpc-message", Value: encodeGrpcMessage(eas.status.Message())},
	}

	// end-stream 为 true
	if err := l.writeHeader(eas.streamID, true, headerFields, nil); err != nil {
		return err
	}
	if eas.rst {
		if err := l.framer.fr.WriteRSTStream(eas.streamID, http2.ErrCodeNo); err != nil {
			return err
		}
	}
	return nil
}

func (l *loopyWriter) incomingGoAwayHandler(*incomingGoAway) error {
	if l.side == clientSide {
		l.draining = true
		if len(l.estdStreams) == 0 {
			return errors.New("received GOAWAY with no active streams")
		}
	}
	return nil
}

func (l *loopyWriter) goAwayHandler(g *goAway) error {
	// Handling of outgoing GoAway is very specific to side.
	if l.ssGoAwayHandler != nil {
		draining, err := l.ssGoAwayHandler(g)
		if err != nil {
			return err
		}
		l.draining = draining
	}
	return nil
}

func (l *loopyWriter) closeConnectionHandler() error {
	// Exit loopyWriter entirely by returning an error here.  This will lead to
	// the transport closing the connection, and, ultimately, transport
	// closure.
	return ErrConnClosing
}

func (l *loopyWriter) handle(i interface{}) error {
	switch i := i.(type) {
	// 流控 收到窗口变更>更改对应窗口 对应stream如有等待则激活
	case *incomingWindowUpdate:
		return l.incomingWindowUpdateHandler(i)
		// 流控 发送窗口变更报文 直接构造报文待发送
	case *outgoingWindowUpdate:
		return l.outgoingWindowUpdateHandler(i)
		// 设置 收到设置变更
	case *incomingSettings:
		return l.incomingSettingsHandler(i)
		// 设置 发送设置变更 仅仅是发送
	case *outgoingSettings:
		return l.outgoingSettingsHandler(i)
		// 头域报文
	case *headerFrame:
		return l.headerHandler(i)
		// server侧收到 client header则将之注册到est列表中
	case *registerStream:
		return l.registerStreamHandler(i)
		// 关闭stream的时候使用
	case *cleanupStream:
		return l.cleanupStreamHandler(i)
		// ？啥场景下用这个
		// 功能是服务端发送end-stream给对端，服务端 stream 自身的状态不改变
	case *earlyAbortStream:
		return l.earlyAbortStreamHandler(i)
		// 收到断链
		// client收到后会将自身状态设置为draining，server侧不处理
	case *incomingGoAway:
		return l.incomingGoAwayHandler(i)
		// 数据报文
		// 将数据塞入对应stream列表并激活stream(加入到活跃列表)
	case *dataFrame:
		return l.preprocessData(i)
		// 连通性ping报文
	case *ping:
		return l.pingHandler(i)
		// 处理断链报文
		// 调用client/server回调函数并将自身状态设置为draining
	case *goAway:
		return l.goAwayHandler(i)
		// 流控 发送流控
		// 貌似仅仅是读取一个值然后传递给metric并没有发送网络报文
	case *outFlowControlSizeRequest:
		return l.outFlowControlSizeRequestHandler(i)
		// 关闭链接 和上方的goaway有何区别 > 上方的goaway是收到后处理
		// 实际上是返回一个错误出去
	case closeConnection:
		return l.closeConnectionHandler()
	default:
		return fmt.Errorf("transport: unknown control message type %T", i)
	}
}

// 应用http2 setting
func (l *loopyWriter) applySettings(ss []http2.Setting) error {
	for _, s := range ss {
		switch s.ID {
		// 设置初始化窗口 影响所有已建立的stream
		case http2.SettingInitialWindowSize:
			o := l.oiws
			l.oiws = s.Val
			if o < l.oiws {
				// 如果窗口变大，则将等待窗口中的stream激活
				// If the new limit is greater make all depleted streams active.
				for _, stream := range l.estdStreams {
					if stream.state == waitingOnStreamQuota {
						stream.state = active
						l.activeStreams.enqueue(stream)
					}
				}
			}
		// 仅仅是更新参数
		case http2.SettingHeaderTableSize:
			updateHeaderTblSize(l.hEnc, s.Val)
		}
	}
	return nil
}

// processData removes the first stream from active streams, writes out at most 16KB
// of its data and then puts it at the end of activeStreams if there's still more data
// to be sent and stream has some stream-level flow control.
// 第一个响应参数表示 isEmpty
func (l *loopyWriter) processData() (bool, error) {
	// conn flow control 无额度
	if l.sendQuota == 0 {
		return true, nil
	}

	// 从活跃stream列表取出队首
	// activeStreams 这个队列里存放的都是有数据data frame 要发的
	str := l.activeStreams.dequeue() // Remove the first stream.
	if str == nil {
		return true, nil
	}
	// 从stream的发送队列中获取队首数据块
	dataItem := str.itl.peek().(*dataFrame) // Peek at the first data item this stream.
	// A data item is represented by a dataFrame, since it later translates into
	// multiple HTTP2 data frames.
	// Every dataFrame has two buffers; h that keeps grpc-message header and d that is actual data.
	// As an optimization to keep wire traffic low, data from d is copied to h to make as big as the
	// maximum possible HTTP2 frame size.

	// 发送空的数据报文
	if len(dataItem.h) == 0 && len(dataItem.d) == 0 { // Empty data frame
		// Client sends out empty data frame with endStream = true
		if err := l.framer.fr.WriteData(dataItem.streamID, dataItem.endStream, nil); err != nil {
			// 发送失败 此时报文还在队列里
			return false, err
		}

		// 移除该数据块
		str.itl.dequeue() // remove the empty data item from stream
		if str.itl.isEmpty() {
			// 发送队列为空更新状态为空
			str.state = empty
		} else if trailer, ok := str.itl.peek().(*headerFrame); ok { // the next item is trailers.
			// data后面允许再发一个trailer
			if err := l.writeHeader(trailer.streamID, trailer.endStream, trailer.hf, trailer.onWrite); err != nil {
				return false, err
			}
			if err := l.cleanupStreamHandler(trailer.cleanup); err != nil {
				return false, nil
			}
		} else {
			// 如果还是dataframe则重新加入活跃列表下次再发
			l.activeStreams.enqueue(str)
		}
		return false, nil
	}
	var (
		buf []byte
	)
	// Figure out the maximum size we can send
	maxSize := http2MaxFrameLen
	if strQuota := int(l.oiws) - str.bytesOutStanding; strQuota <= 0 { // stream-level flow control.
		// 等待stream 发送额度, 这里为啥使用oiws？
		// 等待对端发送窗口更新
		str.state = waitingOnStreamQuota
		return false, nil
	} else if maxSize > strQuota {
		maxSize = strQuota
	}
	if maxSize > int(l.sendQuota) { // connection-level flow control.
		maxSize = int(l.sendQuota)
	}
	// maxSize必定大于0

	// Compute how much of the header and data we can send within quota and max frame length
	hSize := min(maxSize, len(dataItem.h))
	dSize := min(maxSize-hSize, len(dataItem.d))
	if hSize != 0 {
		if dSize == 0 {
			buf = dataItem.h
		} else {
			// We can add some data to grpc message header to distribute bytes more equally across frames.
			// Copy on the stack to avoid generating garbage
			var localBuf [http2MaxFrameLen]byte
			copy(localBuf[:hSize], dataItem.h)
			copy(localBuf[hSize:], dataItem.d[:dSize])
			buf = localBuf[:hSize+dSize]
		}
	} else {
		buf = dataItem.d
	}

	size := hSize + dSize

	// Now that outgoing flow controls are checked we can replenish str's write quota
	// 这一段作何用
	str.wq.replenish(size)

	var endStream bool
	// If this is the last data message on this stream and all of it can be written in this iteration.
	if dataItem.endStream && len(dataItem.h)+len(dataItem.d) <= size {
		endStream = true
	}
	if dataItem.onEachWrite != nil {
		dataItem.onEachWrite()
	}
	if err := l.framer.fr.WriteData(dataItem.streamID, endStream, buf[:size]); err != nil {
		return false, err
	}
	// 记录发出的数量
	str.bytesOutStanding += size

	// connection level flow control
	l.sendQuota -= uint32(size)
	dataItem.h = dataItem.h[hSize:]
	dataItem.d = dataItem.d[dSize:]

	if len(dataItem.h) == 0 && len(dataItem.d) == 0 { // All the data from that message was written out.
		str.itl.dequeue()
	}
	if str.itl.isEmpty() {
		str.state = empty
	} else if trailer, ok := str.itl.peek().(*headerFrame); ok { // The next item is trailers.
		// 关于grpc trailer https://taoshu.in/grpc-trailers.html
		if err := l.writeHeader(trailer.streamID, trailer.endStream, trailer.hf, trailer.onWrite); err != nil {
			return false, err
		}
		if err := l.cleanupStreamHandler(trailer.cleanup); err != nil {
			return false, err
		}
	} else if int(l.oiws)-str.bytesOutStanding <= 0 { // Ran out of stream quota.
		// stream  conn flwo control
		str.state = waitingOnStreamQuota
	} else { // Otherwise add it back to the list of active streams.
		// 还有报文 那就添加到活跃stream 队 列待处理
		l.activeStreams.enqueue(str)
	}
	return false, nil
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
