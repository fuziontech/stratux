/*
	Copyright (c) 2015-2016 Christopher Young
	Distributable under the terms of The "BSD New" License
	that can be found in the LICENSE file, herein included
	as part of this header.

	uibroadcast.go: Helper functions for managementinterface - notification channels for update "subscriptions"
	 (used for weather and traffic websockets).
*/

package main

import (
	"encoding/json"
	"log"
	"sync"
	"time"

	"golang.org/x/net/websocket"
)

type uibroadcaster struct {
	sockets    []*websocket.Conn
	sockets_mu *sync.Mutex
	messages   chan []byte
}

func NewUIBroadcaster() *uibroadcaster {
	ret := &uibroadcaster{
		sockets:    make([]*websocket.Conn, 0),
		sockets_mu: &sync.Mutex{},
		messages:   make(chan []byte, 1024),
	}
	go ret.writer()
	return ret
}

func (u *uibroadcaster) Send(msg []byte) {
	if u == nil || u.messages == nil {
		return
	}
	select {
	case u.messages <- msg:
	default:
		// Drop rather than stalling RF decoders or callers holding locks.
	}
}

func (u *uibroadcaster) SendJSON(i any) {
	if u == nil || u.messages == nil {
		return
	}
	j, err := json.Marshal(i)
	if err != nil {
		log.Printf("uibroadcaster: json.Marshal failed: %v\n", err)
		return
	}
	u.Send(j)
}

func (u *uibroadcaster) AddSocket(sock *websocket.Conn) {
	if u == nil || sock == nil {
		return
	}
	u.sockets_mu.Lock()
	u.sockets = append(u.sockets, sock)
	u.sockets_mu.Unlock()
}

func (u *uibroadcaster) writer() {
	for msg := range u.messages {
		u.sockets_mu.Lock()
		if len(u.sockets) == 0 {
			u.sockets_mu.Unlock()
			continue
		}
		snapshot := make([]*websocket.Conn, len(u.sockets))
		copy(snapshot, u.sockets)
		u.sockets_mu.Unlock()

		var failed map[*websocket.Conn]struct{}
		for _, sock := range snapshot {
			err := sock.SetWriteDeadline(time.Now().Add(time.Second))
			var err2 error
			if err == nil {
				_, err2 = sock.Write(msg)
			}
			if err != nil || err2 != nil {
				if failed == nil {
					failed = make(map[*websocket.Conn]struct{})
				}
				failed[sock] = struct{}{}
			}
		}

		if len(failed) > 0 {
			u.sockets_mu.Lock()
			kept := u.sockets[:0]
			for _, sock := range u.sockets {
				if _, isFailed := failed[sock]; !isFailed {
					kept = append(kept, sock)
				}
			}
			for i := len(kept); i < len(u.sockets); i++ {
				u.sockets[i] = nil
			}
			u.sockets = kept
			u.sockets_mu.Unlock()

			for sock := range failed {
				_ = sock.Close()
			}
		}
	}
}
