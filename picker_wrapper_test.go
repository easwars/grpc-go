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
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/grpc/balancer"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/connectivity"
	"google.golang.org/grpc/internal/transport"
	"google.golang.org/grpc/status"
)

const goroutineCount = 5

var (
	testT  = &testTransport{}
	testSC = &acBalancerWrapper{ac: &addrConn{
		state:     connectivity.Ready,
		transport: testT,
	}}
	testSCNotReady = &acBalancerWrapper{ac: &addrConn{
		state: connectivity.TransientFailure,
	}}
)

type testTransport struct {
	transport.ClientTransport
}

type testingPicker struct {
	err       error
	sc        balancer.SubConn
	maxCalled int64
}

func (p *testingPicker) Pick(info balancer.PickInfo) (balancer.PickResult, error) {
	if atomic.AddInt64(&p.maxCalled, -1) < 0 {
		return balancer.PickResult{}, fmt.Errorf("pick called to many times (> goroutineCount)")
	}
	if p.err != nil {
		return balancer.PickResult{}, p.err
	}
	return balancer.PickResult{SubConn: p.sc}, nil
}

func (s) TestBlockingPickTimeout(t *testing.T) {
	bp := newPickerWrapper(nil)
	ctx, cancel := context.WithTimeout(context.Background(), time.Millisecond)
	defer cancel()
	if _, _, err := bp.pick(ctx, true, balancer.PickInfo{}); status.Code(err) != codes.DeadlineExceeded {
		t.Errorf("bp.pick returned error %v, want DeadlineExceeded", err)
	}
}

func (s) TestBlockingPick(t *testing.T) {
	bp := newPickerWrapper(nil)
	// All goroutines should block because picker is nil in bp.
	var finishedCount uint64
	ctx, cancel := context.WithTimeout(context.Background(), defaultTestTimeout)
	defer cancel()
	for i := goroutineCount; i > 0; i-- {
		go func() {
			if tr, _, err := bp.pick(ctx, true, balancer.PickInfo{}); err != nil || tr != testT {
				t.Errorf("bp.pick returned non-nil error: %v", err)
			}
			atomic.AddUint64(&finishedCount, 1)
		}()
	}
	time.Sleep(50 * time.Millisecond)
	if c := atomic.LoadUint64(&finishedCount); c != 0 {
		t.Errorf("finished goroutines count: %v, want 0", c)
	}
	bp.updatePicker(&testingPicker{sc: testSC, maxCalled: goroutineCount})
}

func (s) TestBlockingPickNoSubAvailable(t *testing.T) {
	bp := newPickerWrapper(nil)
	var finishedCount uint64
	bp.updatePicker(&testingPicker{err: balancer.ErrNoSubConnAvailable, maxCalled: goroutineCount})
	ctx, cancel := context.WithTimeout(context.Background(), defaultTestTimeout)
	defer cancel()
	// All goroutines should block because picker returns no subConn available.
	for i := goroutineCount; i > 0; i-- {
		go func() {
			if tr, _, err := bp.pick(ctx, true, balancer.PickInfo{}); err != nil || tr != testT {
				t.Errorf("bp.pick returned non-nil error: %v", err)
			}
			atomic.AddUint64(&finishedCount, 1)
		}()
	}
	time.Sleep(50 * time.Millisecond)
	if c := atomic.LoadUint64(&finishedCount); c != 0 {
		t.Errorf("finished goroutines count: %v, want 0", c)
	}
	bp.updatePicker(&testingPicker{sc: testSC, maxCalled: goroutineCount})
}

func (s) TestBlockingPickTransientWaitforready(t *testing.T) {
	bp := newPickerWrapper(nil)
	bp.updatePicker(&testingPicker{err: balancer.ErrTransientFailure, maxCalled: goroutineCount})
	var finishedCount uint64
	ctx, cancel := context.WithTimeout(context.Background(), defaultTestTimeout)
	defer cancel()
	// All goroutines should block because picker returns transientFailure and
	// picks are not failfast.
	for i := goroutineCount; i > 0; i-- {
		go func() {
			if tr, _, err := bp.pick(ctx, false, balancer.PickInfo{}); err != nil || tr != testT {
				t.Errorf("bp.pick returned non-nil error: %v", err)
			}
			atomic.AddUint64(&finishedCount, 1)
		}()
	}
	time.Sleep(time.Millisecond)
	if c := atomic.LoadUint64(&finishedCount); c != 0 {
		t.Errorf("finished goroutines count: %v, want 0", c)
	}
	bp.updatePicker(&testingPicker{sc: testSC, maxCalled: goroutineCount})
}

func (s) TestBlockingPickSCNotReady(t *testing.T) {
	bp := newPickerWrapper(nil)
	bp.updatePicker(&testingPicker{sc: testSCNotReady, maxCalled: goroutineCount})
	var finishedCount uint64
	ctx, cancel := context.WithTimeout(context.Background(), defaultTestTimeout)
	defer cancel()
	// All goroutines should block because subConn is not ready.
	for i := goroutineCount; i > 0; i-- {
		go func() {
			if tr, _, err := bp.pick(ctx, true, balancer.PickInfo{}); err != nil || tr != testT {
				t.Errorf("bp.pick returned non-nil error: %v", err)
			}
			atomic.AddUint64(&finishedCount, 1)
		}()
	}
	time.Sleep(time.Millisecond)
	if c := atomic.LoadUint64(&finishedCount); c != 0 {
		t.Errorf("finished goroutines count: %v, want 0", c)
	}
	bp.updatePicker(&testingPicker{sc: testSC, maxCalled: goroutineCount})
}
