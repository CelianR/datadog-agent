// Unless explicitly stated otherwise all files in this repository are licensed
// under the Apache License Version 2.0.
// This product includes software developed at Datadog (https://www.datadoghq.com/).
// Copyright 2016-present Datadog, Inc.

package api

import (
	"errors"
	"net"
	"sync"
	"time"

	"go.uber.org/atomic"

	"github.com/DataDog/datadog-agent/pkg/trace/log"
	"github.com/DataDog/datadog-go/v5/statsd"
)

// measuredListener wraps an existing net.Listener and emits metrics upon accepting connections.
type measuredListener struct {
	net.Listener

	name     string         // metric name to emit
	accepted *atomic.Uint32 // accepted connection count
	timedout *atomic.Uint32 // timedout connection count
	errored  *atomic.Uint32 // errored connection count
	exit     chan struct{}  // exit signal channel (on Close call)
	sem      chan struct{}  // Used to limit active connections
	stop     sync.Once
	statsd   statsd.ClientInterface
}

// NewMeasuredListener wraps ln and emits metrics every 10 seconds. The metric name is
// datadog.trace_agent.receiver.<name>. Additionally, a "status" tag will be added with
// potential values "accepted", "timedout" or "errored".
func NewMeasuredListener(ln net.Listener, name string, maxConn int, statsd statsd.ClientInterface) net.Listener {
	if maxConn == 0 {
		maxConn = 1
	}
	log.Infof("Listener started with %d maximum connections.", maxConn)
	ml := &measuredListener{
		Listener: ln,
		name:     "datadog.trace_agent.receiver." + name,
		accepted: atomic.NewUint32(0),
		timedout: atomic.NewUint32(0),
		errored:  atomic.NewUint32(0),
		exit:     make(chan struct{}),
		sem:      make(chan struct{}, maxConn),
		statsd:   statsd,
	}
	go ml.run()
	return ml
}

func (ln *measuredListener) run() {
	tick := time.NewTicker(10 * time.Second)
	defer tick.Stop()
	for {
		select {
		case <-tick.C:
			ln.flushMetrics()
		case <-ln.exit:
			return
		}
	}
}

func (ln *measuredListener) flushMetrics() {
	for tag, stat := range map[string]*atomic.Uint32{
		"status:accepted": ln.accepted,
		"status:timedout": ln.timedout,
		"status:errored":  ln.errored,
	} {
		if v := int64(stat.Swap(0)); v > 0 {
			_ = ln.statsd.Count(ln.name, v, []string{tag}, 1)
		}
	}
}

type onCloseConn struct {
	net.Conn
	onClose   func()
	closeOnce sync.Once
}

func (c *onCloseConn) Close() error {
	var err error
	c.closeOnce.Do(func() {
		err = c.Conn.Close()
		c.onClose()
	})
	return err
}

//nolint:revive // TODO(APM) Fix revive linter
func OnCloseConn(c net.Conn, onclose func()) net.Conn {
	return &onCloseConn{c, onclose, sync.Once{}}
}

// Accept implements net.Listener and keeps counts on connection statuses.
func (ln *measuredListener) Accept() (net.Conn, error) {
	ln.sem <- struct{}{}
	conn, err := ln.Listener.Accept()
	if err != nil {
		log.Debugf("Error connection named %q: %s", ln.name, err)
		if ne, ok := err.(net.Error); ok && ne.Timeout() && !ne.Temporary() {
			ln.timedout.Inc()
		} else {
			ln.errored.Inc()
		}
	} else {
		ln.accepted.Inc()
		log.Tracef("Accepted connection named %q.", ln.name)
	}
	conn = OnCloseConn(conn, func() {
		<-ln.sem
	})
	return conn, err
}

// Close implements net.Listener.
func (ln *measuredListener) Close() error {
	err := ln.Listener.Close()
	ln.flushMetrics()
	ln.stop.Do(func() {
		close(ln.exit)
	})
	return err
}

// Addr implements net.Listener.
func (ln *measuredListener) Addr() net.Addr { return ln.Listener.Addr() }

// rateLimitedListener wraps a regular TCPListener with rate limiting.
type rateLimitedListener struct {
	*net.TCPListener

	lease  *atomic.Int32  // connections allowed until refresh
	exit   chan struct{}  // exit notification channel
	closed *atomic.Uint32 // closed will be non-zero if the listener was closed

	// stats
	accepted *atomic.Uint32
	rejected *atomic.Uint32
	timedout *atomic.Uint32
	errored  *atomic.Uint32

	statsd statsd.ClientInterface
}

// newRateLimitedListener returns a new wrapped listener, which is non-initialized
func newRateLimitedListener(l net.Listener, conns int, statsd statsd.ClientInterface) (*rateLimitedListener, error) {
	tcpL, ok := l.(*net.TCPListener)

	if !ok {
		return nil, errors.New("cannot wrap listener")
	}

	return &rateLimitedListener{
		lease:       atomic.NewInt32(int32(conns)),
		TCPListener: tcpL,
		exit:        make(chan struct{}),
		closed:      atomic.NewUint32(0),
		accepted:    atomic.NewUint32(0),
		rejected:    atomic.NewUint32(0),
		timedout:    atomic.NewUint32(0),
		errored:     atomic.NewUint32(0),
		statsd:      statsd,
	}, nil
}

// Refresh periodically refreshes the connection lease, and thus cancels any rate limits in place
func (sl *rateLimitedListener) Refresh(conns int) {
	defer close(sl.exit)

	t := time.NewTicker(30 * time.Second)
	defer t.Stop()
	tickStats := time.NewTicker(10 * time.Second)
	defer tickStats.Stop()

	for {
		select {
		case <-sl.exit:
			return
		case <-tickStats.C:
			for tag, stat := range map[string]*atomic.Uint32{
				"status:accepted": sl.accepted,
				"status:rejected": sl.rejected,
				"status:timedout": sl.timedout,
				"status:errored":  sl.errored,
			} {
				v := int64(stat.Swap(0))
				_ = sl.statsd.Count("datadog.trace_agent.receiver.tcp_connections", v, []string{tag}, 1)
			}
		case <-t.C:
			sl.lease.Store(int32(conns))
			log.Debugf("Refreshed the connection lease: %d conns available", conns)
		}
	}
}

// rateLimitedError  indicates a user request being blocked by our rate limit
// It satisfies the net.Error interface
type rateLimitedError struct{}

// Error returns an error string
func (e *rateLimitedError) Error() string { return "request has been rate-limited" }

// Temporary tells the HTTP server loop that this error is temporary and recoverable
func (e *rateLimitedError) Temporary() bool { return true }

// Timeout tells the HTTP server loop that this error is not a timeout
func (e *rateLimitedError) Timeout() bool { return false }

// Accept reimplements the regular Accept but adds rate limiting.
func (sl *rateLimitedListener) Accept() (net.Conn, error) {
	if sl.lease.Load() <= 0 {
		// we've reached our cap for this lease period; reject the request
		sl.rejected.Inc()
		return nil, &rateLimitedError{}
	}
	for {
		// ensure potential TCP handshake timeouts don't stall us forever
		if err := sl.SetDeadline(time.Now().Add(time.Second)); err != nil {
			log.Debugf("Error setting rate limiter deadline: %v", err)
		}
		conn, err := sl.TCPListener.Accept()
		if err != nil {
			if ne, ok := err.(net.Error); ok && ne.Timeout() {
				if ne.Temporary() {
					// deadline expired; continue
					continue
				} else { //nolint:revive // TODO(APM) Fix revive linter
					// don't count temporary errors; they usually signify expired deadlines
					// see (golang/go/src/internal/poll/fd.go).TimeoutError
					sl.timedout.Inc()
				}
			} else {
				sl.errored.Inc()
			}
			return conn, err
		}

		sl.lease.Dec()
		sl.accepted.Inc()

		return conn, nil
	}
}

// Close wraps the Close method of the underlying tcp listener
func (sl *rateLimitedListener) Close() error {
	if !sl.closed.CompareAndSwap(0, 1) {
		// already closed
		return nil
	}
	sl.exit <- struct{}{}
	<-sl.exit
	return sl.TCPListener.Close()
}
