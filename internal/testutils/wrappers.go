/*
 *
 * Copyright 2022 gRPC authors.
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

package testutils

import (
	"net"
	"testing"
)

// ConnWrapper wraps a net.Conn and pushes on a channel when closed.
type ConnWrapper struct {
	net.Conn
	CloseCh *Channel
}

// Close closes the connection and sends a value on the close channel.
func (cw *ConnWrapper) Close() error {
	err := cw.Conn.Close()
	cw.CloseCh.Replace(nil)
	return err
}

// ListenerWrapper wraps a net.Listener and the returned net.Conn.
type ListenerWrapper struct {
	net.Listener
	NewConnCh *Channel // Used to push accepted connections.
	CloseCh   *Channel // Used to signal close.
}

// Accept accepts a connection on the underlying net.Listener and sends the
// accepted connection on the `NewConnCh` channel.
func (l *ListenerWrapper) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	closeCh := NewChannel()
	conn := &ConnWrapper{Conn: c, CloseCh: closeCh}
	l.NewConnCh.Send(conn)
	return conn, nil
}

// Close closes the underlying net.Listener and sends a notification on the
// `CloseCh` channel.
func (l *ListenerWrapper) Close() error {
	l.CloseCh.Send(nil)
	return l.Listener.Close()
}

// NewListenerWrapper returns a ListenerWrapper.
func NewListenerWrapper(t *testing.T, lis net.Listener) *ListenerWrapper {
	if lis == nil {
		var err error
		lis, err = LocalTCPListener()
		if err != nil {
			t.Fatal(err)
		}
	}

	return &ListenerWrapper{
		Listener:  lis,
		NewConnCh: NewChannel(),
		CloseCh:   NewChannel(),
	}
}
