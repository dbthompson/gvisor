// Copyright 2021 The gVisor Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package lisafs

import (
	"path/filepath"
	"syscall"

	"gvisor.dev/gvisor/pkg/flipcall"
	"gvisor.dev/gvisor/pkg/log"
	"gvisor.dev/gvisor/pkg/marshal"
	"gvisor.dev/gvisor/pkg/p9"
	"gvisor.dev/gvisor/pkg/sync"
	"gvisor.dev/gvisor/pkg/unet"
)

// ConnectionManager manages the lifetime (from creation to destruction) of
// connections in the gofer process. It is also responsible for server sharing.
type ConnectionManager struct {
	// servers contains a mapping between a server and the path at which it was
	// mounted. The path must be filepath.Clean()'d. It is protected by serversMu.
	// TODO(ayushranjan): Are symlinks even possible?
	servers   map[string]*Server
	serversMu sync.Mutex

	wg sync.WaitGroup
}

// StartConnection starts a new connection.
func (cm *ConnectionManager) StartConnection(sock *unet.Socket, hostPath string, handlers []RPCHanlder, connOpts interface{}) error {
	hostPath = filepath.Clean(hostPath)

	var s *Server
	cm.serversMu.Lock()
	if cm.servers == nil {
		cm.servers = make(map[string]*Server)
	}
	if s = cm.servers[hostPath]; s == nil {
		s = newServer(hostPath)
		cm.servers[hostPath] = s
	}
	cm.serversMu.Unlock()

	c := &Connection{
		server:   s,
		sock:     sock,
		handlers: handlers,
		opts:     connOpts,
		channels: make([]*channel, 0, maxChannels),
	}

	alloc, err := flipcall.NewPacketWindowAllocator()
	if err != nil {
		return err
	}
	c.channelAlloc = alloc

	// Each connection has its dedicated goroutine.
	cm.wg.Add(1)
	go func() {
		c.run()
		cm.wg.Done()
	}()
	return nil
}

// Wait waits for all connections to terminate.
func (cm *ConnectionManager) Wait() {
	cm.wg.Wait()
}

// Connection represents a connection between a gofer mount in the sentry and
// the gofer process. This is owned by the gofer process and facilitates
// communication with the Client.
type Connection struct {
	// server serves a filesystem tree that this connection is immutably
	// associated with. This server might be shared across connections. This
	// helps when we have bind mounts that are shared between containers in a
	// runsc pod.
	server *Server

	// sock is the main socket by which this connections is established.
	sock *unet.Socket

	// handlers contains all the message handlers which is defined by the server
	// implementation. It is indexed by MID. handlers is immutable. If
	// handlers[MID] is nil, then that MID is not supported.
	handlers []RPCHanlder

	// channelsMu protects channels.
	channelsMu sync.Mutex
	// channels keeps track of all open channels.
	channels []*channel

	// activeWg represents active channels.
	activeWg sync.WaitGroup

	// pendingWg represents channels with pending requests.
	pendingWg sync.WaitGroup

	// channelAlloc is used to allocate memory for channels.
	channelAlloc *flipcall.PacketWindowAllocator

	// fds keeps tracks of open FDs on this server. It is protected by fdsMu.
	fds map[FDID]FD
	// nextFDID is the next available FDID. It is protected by fdsMu.
	nextFDID FDID
	fdsMu    sync.RWMutex

	// opts is the connection specific options and is immutable. This is supplied
	// to all operations and specific to a gofer implementation.
	opts interface{}
}

// Server returns the server serving this connection.
func (c *Connection) Server() *Server {
	return c.server
}

// run defines the lifecycle of a connection.
func (c *Connection) run() {
	defer c.close()

	// Start handling requests on this connection.
	for {
		m, payload, err := readMessageFrom(c.sock, nil)
		if err != nil {
			log.Debugf("sock read failed, closing connection: %v", err)
			return
		}
		respM, respMsg, respFDs := c.handleMsg(m, payload)
		if err := writeMessageTo(c.sock, respM, respMsg, respFDs); err != nil {
			log.Debugf("sock write failed, closing connection: %v", err)
			return
		}
	}
}

// service starts servicing the passed channel until the channel is shutdown.
// This is a blocking method and hence must be called in a separate goroutine.
func (c *Connection) service(ch *channel) error {
	rcvDataLen, err := ch.data.RecvFirst()
	if err != nil {
		return err
	}
	for rcvDataLen > 0 {
		m, payload, err := ch.readMsg(rcvDataLen, nil)
		if err != nil {
			return err
		}
		respM, respMsg, respFDs := c.handleMsg(m, payload)
		sndDataLen, err := ch.writeMsg(respM, respMsg, respFDs)
		if err != nil {
			return err
		}
		rcvDataLen, err = ch.data.SendRecv(sndDataLen)
		if err != nil {
			return err
		}
	}
	return nil
}

func (c *Connection) handleMsg(m MID, payload []byte) (MID, marshal.Marshallable, []int) {
	c.pendingWg.Add(1)
	defer c.pendingWg.Done()

	// Check if the message is supported.
	if int(m) >= len(c.handlers) || c.handlers[m] == nil {
		log.Warningf("received request which is not supported by the server, MID = %d", m)
		resp := &ErrorRes{errno: uint32(syscall.EOPNOTSUPP)}
		return Error, resp, nil
	}

	// Try handling the request.
	resp, fds, err := c.handlers[m](c, payload)
	if err != nil {
		resp = &ErrorRes{errno: uint32(p9.ExtractErrno(err))}
		return Error, resp, nil
	}

	return m, resp, fds
}

func (c *Connection) close() {
	// Wait for completion of all inflight requests. This is mostly so that if
	// a request is stuck, the sandbox supervisor has the opportunity to kill
	// us with SIGABRT to get a stack dump of the offending handler.
	c.pendingWg.Wait()

	// Shutdown and clean up channels.
	c.channelsMu.Lock()
	for _, ch := range c.channels {
		ch.shutdown()
	}
	c.activeWg.Wait()
	for _, ch := range c.channels {
		ch.destroy()
	}
	// This is to prevent additional channels from being created.
	c.channels = nil
	c.channelsMu.Unlock()

	// Free the channel memory.
	if c.channelAlloc != nil {
		c.channelAlloc.Destroy()
	}

	// Ensure the connection is closed.
	c.sock.Close()

	// Cleanup all FDs.
	c.fdsMu.Lock()
	for fdid := range c.fds {
		c.removeFDLocked(fdid)
	}
	c.fdsMu.Unlock()
}

// LookupFD retrives the FD identified by id on this connection. This operation
// increments the returned FD's ref which is owned by the caller.
func (c *Connection) LookupFD(id FDID) (FD, error) {
	c.fdsMu.RLock()
	defer c.fdsMu.RUnlock()

	fd, ok := c.fds[id]
	if !ok {
		return nil, syscall.EBADF
	}
	fd.IncRef()
	return fd, nil
}

// InsertFD inserts the passed fd into the internal datastructure to track FDs.
// The caller must hold a ref on fd which is transferred to the connection.
func (c *Connection) InsertFD(fd FD) FDID {
	c.fdsMu.Lock()
	defer c.fdsMu.Unlock()

	res := c.nextFDID
	c.nextFDID++
	if c.nextFDID < res {
		panic("ran out of FDIDs")
	}
	c.fds[res] = fd
	return res
}

// RemoveFD makes c stop tracking the passed FDID and drops its ref on the FD.
func (c *Connection) RemoveFD(id FDID) error {
	c.fdsMu.Lock()
	defer c.fdsMu.Unlock()

	return c.removeFDLocked(id)
}

func (c *Connection) removeFDLocked(id FDID) error {
	fd, ok := c.fds[id]
	if !ok {
		return syscall.EBADF
	}
	delete(c.fds, id)
	fd.DecRef(nil)
	return nil
}

// UnsupportedMessages returns all message IDs that are not supported on this
// connection. An MID is unsupported if handlers[MID] == nil.
func (c *Connection) UnsupportedMessages() []MID {
	var res []MID
	for i := range c.handlers {
		if c.handlers[i] == nil {
			res = append(res, MID(i))
		}
	}
	return res
}

// MaxMessage is the maximum message ID supported on this connection.
func (c *Connection) MaxMessage() MID {
	return MID(len(c.handlers) - 1)
}
