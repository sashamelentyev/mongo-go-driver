// Copyright (C) MongoDB, Inc. 2017-present.
//
// Licensed under the Apache License, Version 2.0 (the "License"); you may
// not use this file except in compliance with the License. You may obtain
// a copy of the License at http://www.apache.org/licenses/LICENSE-2.0

package topology

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"strings"

	"go.mongodb.org/mongo-driver/x/mongo/driver"
	"go.mongodb.org/mongo-driver/x/network/address"
	"go.mongodb.org/mongo-driver/x/network/command"
	connectionlegacy "go.mongodb.org/mongo-driver/x/network/connection"
	"go.mongodb.org/mongo-driver/x/network/description"
	"go.mongodb.org/mongo-driver/x/network/wiremessage"
)

var globalConnectionID uint64

func nextConnectionID() uint64 { return atomic.AddUint64(&globalConnectionID, 1) }

type connection struct {
	id               string
	nc               net.Conn // When nil, the connection is closed.
	addr             address.Address
	idleTimeout      time.Duration
	idleDeadline     time.Time
	lifetimeDeadline time.Time
	readTimeout      time.Duration
	writeTimeout     time.Duration
	desc             description.Server

	// pool related fields
	pool       *pool
	poolID     uint64
	generation uint64
}

// newConnection handles the creation of a connection. It will dial, configure TLS, and perform
// initialization handshakes.
func newConnection(ctx context.Context, addr address.Address, opts ...ConnectionOption) (*connection, error) {
	cfg, err := newConnectionConfig(opts...)
	if err != nil {
		return nil, err
	}

	nc, err := cfg.dialer.DialContext(ctx, addr.Network(), addr.String())
	if err != nil {
		return nil, err
	}

	if cfg.tlsConfig != nil {
		tlsConfig := cfg.tlsConfig.Clone()
		nc, err = configureTLS(ctx, nc, addr, tlsConfig)
		if err != nil {
			return nil, err
		}
	}

	var lifetimeDeadline time.Time
	if cfg.lifeTimeout > 0 {
		lifetimeDeadline = time.Now().Add(cfg.lifeTimeout)
	}

	id := fmt.Sprintf("%s[-%d]", addr, nextConnectionID())

	c := &connection{
		id:               id,
		nc:               nc,
		addr:             addr,
		idleTimeout:      cfg.idleTimeout,
		lifetimeDeadline: lifetimeDeadline,
		readTimeout:      cfg.readTimeout,
		writeTimeout:     cfg.writeTimeout,
	}

	c.bumpIdleDeadline()

	// running isMaster and authentication is handled by a handshaker on the configuration instance.
	if cfg.handshaker != nil {
		c.desc, err = cfg.handshaker.Handshake(ctx, c.addr, initConnection{c})
		if err != nil {
			return nil, err
		}
	}
	return c, nil
}

func (c *connection) writeWireMessage(ctx context.Context, wm []byte) error {
	var err error
	if c.nc == nil {
		return ConnectionError{ConnectionID: c.id, message: "connection is closed"}
	}
	select {
	case <-ctx.Done():
		return ConnectionError{ConnectionID: c.id, Wrapped: ctx.Err(), message: "failed to write"}
	default:
	}

	var deadline time.Time
	if c.writeTimeout != 0 {
		deadline = time.Now().Add(c.writeTimeout)
	}

	if dl, ok := ctx.Deadline(); ok && (deadline.IsZero() || dl.Before(deadline)) {
		deadline = dl
	}

	if err := c.nc.SetWriteDeadline(deadline); err != nil {
		return ConnectionError{ConnectionID: c.id, Wrapped: err, message: "failed to set write deadline"}
	}

	_, err = c.nc.Write(wm)
	if err != nil {
		// TODO(GODRIVER-929): Close connection through the pool.
		_ = c.nc.Close()
		c.nc = nil
		return ConnectionError{ConnectionID: c.id, Wrapped: err, message: "unable to write wire message to network"}
	}

	c.bumpIdleDeadline()
	return nil
}

// readWireMessage reads a wiremessage from the connection. The dst parameter will be overwritten.
func (c *connection) readWireMessage(ctx context.Context, dst []byte) ([]byte, error) {
	if c.nc == nil {
		return dst, ConnectionError{ConnectionID: c.id, message: "connection is closed"}
	}

	select {
	case <-ctx.Done():
		// We close the connection because we don't know if there is an unread message on the wire.
		// TODO(GODRIVER-929): Close connection through the pool.
		_ = c.nc.Close()
		c.nc = nil
		return nil, ConnectionError{ConnectionID: c.id, Wrapped: ctx.Err(), message: "failed to read"}
	default:
	}

	var deadline time.Time
	if c.readTimeout != 0 {
		deadline = time.Now().Add(c.readTimeout)
	}

	if dl, ok := ctx.Deadline(); ok && (deadline.IsZero() || dl.Before(deadline)) {
		deadline = dl
	}

	if err := c.nc.SetReadDeadline(deadline); err != nil {
		return nil, ConnectionError{ConnectionID: c.id, Wrapped: err, message: "failed to set read deadline"}
	}

	// We use an array here because it only costs 4 bytes on the stack and means we'll only need to
	// reslice dst once instead of twice.
	var sizeBuf [4]byte

	// We do a ReadFull into an array here instead of doing an opportunistic ReadAtLeast into dst
	// because there might be more than one wire message waiting to be read, for example when
	// reading messages from an exhaust cursor.
	_, err := io.ReadFull(c.nc, sizeBuf[:])
	if err != nil {
		// We close the connection because we don't know if there are other bytes left to read.
		// TODO(GODRIVER-929): Close connection through the pool.
		_ = c.nc.Close()
		c.nc = nil
		return nil, ConnectionError{ConnectionID: c.id, Wrapped: err, message: "unable to decode message length"}
	}

	// read the length as an int32
	size := (int32(sizeBuf[0])) | (int32(sizeBuf[1]) << 8) | (int32(sizeBuf[2]) << 16) | (int32(sizeBuf[3]) << 24)

	if int(size) > cap(dst) {
		// Since we can't grow this slice without allocating, just allocate an entirely new slice.
		dst = make([]byte, 0, size)
	}
	// We need to ensure we don't accidentally read into a subsequent wire message, so we set the
	// size to read exactly this wire message.
	dst = dst[:size]
	copy(dst, sizeBuf[:])

	_, err = io.ReadFull(c.nc, dst[4:])
	if err != nil {
		// We close the connection because we don't know if there are other bytes left to read.
		// TODO(GODRIVER-929): Close connection through the pool.
		_ = c.nc.Close()
		c.nc = nil
		return nil, ConnectionError{ConnectionID: c.id, Wrapped: err, message: "unable to read full message"}
	}

	c.bumpIdleDeadline()
	return dst, nil
}

func (c *connection) expired() bool {
	now := time.Now()
	if !c.idleDeadline.IsZero() && now.After(c.idleDeadline) {
		return true
	}

	if !c.lifetimeDeadline.IsZero() && now.After(c.lifetimeDeadline) {
		return true
	}

	return c.nc == nil
}

func (c *connection) bumpIdleDeadline() {
	if c.idleTimeout > 0 {
		c.idleDeadline = time.Now().Add(c.idleTimeout)
	}
}

// initConnection is an adapter used during connection initialization. It has the minimum
// functionality necessary to implement the driver.Connection interface, which is required to pass a
// *connection to a Handshaker.
type initConnection struct{ *connection }

var _ driver.Connection = initConnection{}

func (c initConnection) Description() description.Server { return description.Server{} }
func (c initConnection) Close() error                    { return c.nc.Close() }
func (c initConnection) ID() string                      { return c.id }
func (c initConnection) Address() address.Address        { return c.addr }
func (c initConnection) WriteWireMessage(ctx context.Context, wm []byte) error {
	return c.writeWireMessage(ctx, wm)
}
func (c initConnection) ReadWireMessage(ctx context.Context, dst []byte) ([]byte, error) {
	return c.readWireMessage(ctx, dst)
}

// Connection implements the driver.Connection interface. It allows reading and writing wire
// messages.
type Connection struct {
	*connection
	s *Server

	mu sync.RWMutex
}

var _ driver.Connection = (*Connection)(nil)

// WriteWireMessage handles writing a wire message to the underlying connection.
func (c *Connection) WriteWireMessage(ctx context.Context, wm []byte) error {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.connection == nil {
		return ErrConnectionClosed
	}
	return c.writeWireMessage(ctx, wm)
}

// ReadWireMessage handles reading a wire message from the underlying connection. The dst parameter
// will be overwritten with the new wire message.
func (c *Connection) ReadWireMessage(ctx context.Context, dst []byte) ([]byte, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.connection == nil {
		return dst, ErrConnectionClosed
	}
	return c.readWireMessage(ctx, dst)
}

// Description returns the server description of the server this connection is connected to.
func (c *Connection) Description() description.Server {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.connection == nil {
		return description.Server{}
	}
	return c.desc
}

// Close returns this connection to the connection pool. This method may not close the underlying
// socket.
func (c *Connection) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.connection == nil {
		return nil
	}
	// TODO(GODRIVER-932): Release an entry in the semaphore.
	err := c.pool.put(c.connection)
	if err != nil {
		return err
	}
	c.connection = nil
	return nil
}

// ID returns the ID of this connection.
func (c *Connection) ID() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.connection == nil {
		return "<closed>"
	}
	return c.id
}

// Address returns the address of this connection.
func (c *Connection) Address() address.Address {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.connection == nil {
		return address.Address("0.0.0.0")
	}
	return c.addr
}

// sconn is a wrapper around a connection.Connection. This type is returned by
// a Server so that it can track network errors and when a non-timeout network
// error is returned, the pool on the server can be cleared.
type sconn struct {
	connectionlegacy.Connection
	s  *Server
	id uint64
}

var notMasterCodes = []int32{10107, 13435}
var recoveringCodes = []int32{11600, 11602, 13436, 189, 91}

func (sc *sconn) ReadWireMessage(ctx context.Context) (wiremessage.WireMessage, error) {
	wm, err := sc.Connection.ReadWireMessage(ctx)
	if err != nil {
		sc.processErr(err)
	} else {
		e := command.DecodeError(wm)
		sc.processErr(e)
	}
	return wm, err
}

func (sc *sconn) WriteWireMessage(ctx context.Context, wm wiremessage.WireMessage) error {
	err := sc.Connection.WriteWireMessage(ctx, wm)
	sc.processErr(err)
	return err
}

func (sc *sconn) processErr(err error) {
	// Invalidate server description if not master or node recovering error occurs
	if cerr, ok := err.(command.Error); ok && (isRecoveringError(cerr) || isNotMasterError(cerr)) {
		desc := sc.s.Description()
		desc.Kind = description.Unknown
		desc.LastError = err
		// updates description to unknown
		sc.s.updateDescription(desc, false)
		sc.s.RequestImmediateCheck()
		_ = sc.s.pool.Drain()
		return
	}

	ne, ok := err.(connectionlegacy.Error)
	if !ok {
		return
	}

	if netErr, ok := ne.Wrapped.(net.Error); ok && netErr.Timeout() {
		return
	}
	if ne.Wrapped == context.Canceled || ne.Wrapped == context.DeadlineExceeded {
		return
	}

	desc := sc.s.Description()
	desc.Kind = description.Unknown
	desc.LastError = err
	// updates description to unknown
	sc.s.updateDescription(desc, false)
}

func isRecoveringError(err command.Error) bool {
	for _, c := range recoveringCodes {
		if c == err.Code {
			return true
		}
	}
	return strings.Contains(err.Error(), "node is recovering")
}

func isNotMasterError(err command.Error) bool {
	for _, c := range notMasterCodes {
		if c == err.Code {
			return true
		}
	}
	return strings.Contains(err.Error(), "not master")
}

func configureTLS(ctx context.Context, nc net.Conn, addr address.Address, config *tls.Config) (net.Conn, error) {
	if !config.InsecureSkipVerify {
		hostname := addr.String()
		colonPos := strings.LastIndex(hostname, ":")
		if colonPos == -1 {
			colonPos = len(hostname)
		}

		hostname = hostname[:colonPos]
		config.ServerName = hostname
	}

	client := tls.Client(nc, config)

	errChan := make(chan error, 1)
	go func() {
		errChan <- client.Handshake()
	}()

	select {
	case err := <-errChan:
		if err != nil {
			return nil, err
		}
	case <-ctx.Done():
		return nil, errors.New("server connection cancelled/timeout during TLS handshake")
	}
	return client, nil
}