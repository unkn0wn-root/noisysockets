// SPDX-License-Identifier: MPL-2.0
/*
 * Copyright (C) 2024 The Noisy Sockets Authors.
 *
 * This Source Code Form is subject to the terms of the Mozilla Public
 * License, v. 2.0. If a copy of the MPL was not distributed with this
 * file, You can obtain one at http://mozilla.org/MPL/2.0/.
 *
 * Portions of this file are based on code originally from wireguard-go,
 *
 * Copyright (C) 2017-2023 WireGuard LLC. All Rights Reserved.
 *
 * Permission is hereby granted, free of charge, to any person obtaining a copy of
 * this software and associated documentation files (the "Software"), to deal in
 * the Software without restriction, including without limitation the rights to
 * use, copy, modify, merge, publish, distribute, sublicense, and/or sell copies
 * of the Software, and to permit persons to whom the Software is furnished to do
 * so, subject to the following conditions:
 *
 * The above copyright notice and this permission notice shall be included in all
 * copies or substantial portions of the Software.
 *
 * THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
 * IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
 * FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
 * AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
 * LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
 * OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE
 * SOFTWARE.
 */

package transport

import (
	"sync"
	"time"
	_ "unsafe"
)

//go:linkname fastrandn runtime.fastrandn
func fastrandn(n uint32) uint32

// A Timer manages time-based aspects of the WireGuard protocol.
// Timer roughly copies the interface of the Linux kernel's struct timer_list.
type Timer struct {
	*time.Timer
	modifyingLock sync.RWMutex
	runningLock   sync.Mutex
	isPending     bool
}

func (peer *Peer) NewTimer(expirationFunction func(*Peer)) *Timer {
	timer := &Timer{}
	timer.Timer = time.AfterFunc(time.Hour, func() {
		timer.runningLock.Lock()
		defer timer.runningLock.Unlock()

		timer.modifyingLock.Lock()
		if !timer.isPending {
			timer.modifyingLock.Unlock()
			return
		}
		timer.isPending = false
		timer.modifyingLock.Unlock()

		expirationFunction(peer)
	})
	timer.Stop()
	return timer
}

func (timer *Timer) Mod(d time.Duration) {
	timer.modifyingLock.Lock()
	timer.isPending = true
	timer.Reset(d)
	timer.modifyingLock.Unlock()
}

func (timer *Timer) Del() {
	timer.modifyingLock.Lock()
	timer.isPending = false
	timer.Stop()
	timer.modifyingLock.Unlock()
}

func (timer *Timer) DelSync() {
	timer.Del()
	timer.runningLock.Lock()
	timer.Del()
	timer.runningLock.Unlock()
}

func (timer *Timer) IsPending() bool {
	timer.modifyingLock.RLock()
	defer timer.modifyingLock.RUnlock()
	return timer.isPending
}

func (peer *Peer) timersActive() bool {
	return peer.isRunning.Load() && peer.transport != nil && peer.transport.isUp()
}

func expiredRetransmitHandshake(peer *Peer) {
	if peer.timers.handshakeAttempts.Load() > MaxTimerHandshakes {
		peer.transport.log.Error("Handshake did not complete after multiple attempts, giving up",
			"peer", peer, "maxAttempts", MaxTimerHandshakes+2)

		if peer.timersActive() {
			peer.timers.sendKeepalive.Del()
		}

		/* We drop all packets without a keypair and don't try again,
		 * if we try unsuccessfully for too long to make a handshake.
		 */
		peer.FlushStagedPackets()

		/* We set a timer for destroying any residue that might be left
		 * of a partial exchange.
		 */
		if peer.timersActive() && !peer.timers.zeroKeyMaterial.IsPending() {
			peer.timers.zeroKeyMaterial.Mod(RejectAfterTime * 3)
		}
	} else {
		peer.timers.handshakeAttempts.Add(1)
		peer.transport.log.Warn("Handshake did not complete within timeout, retrying",
			"peer", peer, "timeout", int(RekeyTimeout.Seconds()), "try", peer.timers.handshakeAttempts.Load()+1)

		if err := peer.SendHandshakeInitiation(true); err != nil {
			peer.transport.log.Error("Failed to retransmit handshake initiation",
				"peer", peer, "error", err)
		}
	}
}

func expiredSendKeepalive(peer *Peer) {
	if err := peer.SendKeepalive(); err != nil {
		peer.transport.log.Error("Failed to send keepalive",
			"peer", peer, "error", err)
	}

	if peer.timers.needAnotherKeepalive.Load() {
		peer.timers.needAnotherKeepalive.Store(false)
		if peer.timersActive() {
			peer.timers.sendKeepalive.Mod(KeepaliveTimeout)
		}
	}
}

func expiredNewHandshake(peer *Peer) {
	peer.transport.log.Debug("Retrying handshake because we stopped hearing back",
		"peer", peer, "timeout", int((KeepaliveTimeout + RekeyTimeout).Seconds()))
	if err := peer.SendHandshakeInitiation(false); err != nil {
		peer.transport.log.Error("Failed to retransmit handshake initiation",
			"peer", peer, "error", err)
	}
}

func expiredZeroKeyMaterial(peer *Peer) {
	peer.transport.log.Debug("Removing all keys, since we haven't received a new one in time",
		"peer", peer, "timeout", int((RejectAfterTime * 3).Seconds()))
	peer.ZeroAndFlushAll()
}

func expiredKeepAlive(peer *Peer) {
	if peer.keepAliveInterval.Load() > 0 {
		if err := peer.SendKeepalive(); err != nil {
			peer.transport.log.Error("Failed to send keepalive",
				"peer", peer, "error", err)
		}
	}
}

/* Should be called after an authenticated data packet is sent. */
func (peer *Peer) timersDataSent() {
	if peer.timersActive() && !peer.timers.newHandshake.IsPending() {
		peer.timers.newHandshake.Mod(KeepaliveTimeout + RekeyTimeout + time.Millisecond*time.Duration(fastrandn(RekeyTimeoutJitterMaxMs)))
	}
}

/* Should be called after an authenticated data packet is received. */
func (peer *Peer) timersDataReceived() {
	if peer.timersActive() {
		if !peer.timers.sendKeepalive.IsPending() {
			peer.timers.sendKeepalive.Mod(KeepaliveTimeout)
		} else {
			peer.timers.needAnotherKeepalive.Store(true)
		}
	}
}

/* Should be called after any type of authenticated packet is sent -- keepalive, data, or handshake. */
func (peer *Peer) timersAnyAuthenticatedPacketSent() {
	if peer.timersActive() {
		peer.timers.sendKeepalive.Del()
	}
}

/* Should be called after any type of authenticated packet is received -- keepalive, data, or handshake. */
func (peer *Peer) timersAnyAuthenticatedPacketReceived() {
	if peer.timersActive() {
		peer.timers.newHandshake.Del()
	}
}

/* Should be called after a handshake initiation message is sent. */
func (peer *Peer) timersHandshakeInitiated() {
	if peer.timersActive() {
		peer.timers.retransmitHandshake.Mod(RekeyTimeout + time.Millisecond*time.Duration(fastrandn(RekeyTimeoutJitterMaxMs)))
	}
}

/* Should be called after a handshake response message is received and processed or when getting key confirmation via the first data message. */
func (peer *Peer) timersHandshakeComplete() {
	if peer.timersActive() {
		peer.timers.retransmitHandshake.Del()
	}
	peer.timers.handshakeAttempts.Store(0)
	peer.timers.sentLastMinuteHandshake.Store(false)
	peer.lastHandshakeNano.Store(time.Now().UnixNano())
}

/* Should be called after an ephemeral key is created, which is before sending a handshake response or after receiving a handshake response. */
func (peer *Peer) timersSessionDerived() {
	if peer.timersActive() {
		peer.timers.zeroKeyMaterial.Mod(RejectAfterTime * 3)
	}
}

/* Should be called before a packet with authentication -- keepalive, data, or handshake -- is sent, or after one is received. */
func (peer *Peer) timersAnyAuthenticatedPacketTraversal() {
	keepAlive := peer.keepAliveInterval.Load()
	if keepAlive > 0 && peer.timersActive() {
		peer.timers.keepAlive.Mod(time.Duration(keepAlive) * time.Second)
	}
}

func (peer *Peer) timersInit() {
	peer.timers.retransmitHandshake = peer.NewTimer(expiredRetransmitHandshake)
	peer.timers.sendKeepalive = peer.NewTimer(expiredSendKeepalive)
	peer.timers.newHandshake = peer.NewTimer(expiredNewHandshake)
	peer.timers.zeroKeyMaterial = peer.NewTimer(expiredZeroKeyMaterial)
	peer.timers.keepAlive = peer.NewTimer(expiredKeepAlive)
}

func (peer *Peer) timersStart() {
	peer.timers.handshakeAttempts.Store(0)
	peer.timers.sentLastMinuteHandshake.Store(false)
	peer.timers.needAnotherKeepalive.Store(false)
}

func (peer *Peer) timersStop() {
	peer.timers.retransmitHandshake.DelSync()
	peer.timers.sendKeepalive.DelSync()
	peer.timers.newHandshake.DelSync()
	peer.timers.zeroKeyMaterial.DelSync()
	peer.timers.keepAlive.DelSync()
}
