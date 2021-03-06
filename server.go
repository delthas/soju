package soju

import (
	"fmt"
	"log"
	"net"
	"sync"
	"time"

	"gopkg.in/irc.v3"
)

// TODO: make configurable
var keepAlivePeriod = time.Minute
var retryConnectMinDelay = time.Minute

func setKeepAlive(c net.Conn) error {
	tcpConn, ok := c.(*net.TCPConn)
	if !ok {
		return fmt.Errorf("cannot enable keep-alive on a non-TCP connection")
	}
	if err := tcpConn.SetKeepAlive(true); err != nil {
		return err
	}
	return tcpConn.SetKeepAlivePeriod(keepAlivePeriod)
}

type Logger interface {
	Print(v ...interface{})
	Printf(format string, v ...interface{})
}

type prefixLogger struct {
	logger Logger
	prefix string
}

var _ Logger = (*prefixLogger)(nil)

func (l *prefixLogger) Print(v ...interface{}) {
	v = append([]interface{}{l.prefix}, v...)
	l.logger.Print(v...)
}

func (l *prefixLogger) Printf(format string, v ...interface{}) {
	v = append([]interface{}{l.prefix}, v...)
	l.logger.Printf("%v"+format, v...)
}

type Server struct {
	Hostname string
	Logger   Logger
	RingCap  int
	Debug    bool

	db *DB

	lock            sync.Mutex
	users           map[string]*user
	downstreamConns []*downstreamConn
}

func NewServer(db *DB) *Server {
	return &Server{
		Logger:  log.New(log.Writer(), "", log.LstdFlags),
		RingCap: 4096,
		users:   make(map[string]*user),
		db:      db,
	}
}

func (s *Server) prefix() *irc.Prefix {
	return &irc.Prefix{Name: s.Hostname}
}

func (s *Server) Run() error {
	users, err := s.db.ListUsers()
	if err != nil {
		return err
	}

	s.lock.Lock()
	for _, record := range users {
		s.Logger.Printf("starting bouncer for user %q", record.Username)
		u := newUser(s, &record)
		s.users[u.Username] = u

		go u.run()
	}
	s.lock.Unlock()

	select {}
}

func (s *Server) getUser(name string) *user {
	s.lock.Lock()
	u := s.users[name]
	s.lock.Unlock()
	return u
}

func (s *Server) Serve(ln net.Listener) error {
	for {
		netConn, err := ln.Accept()
		if err != nil {
			return fmt.Errorf("failed to accept connection: %v", err)
		}

		setKeepAlive(netConn)

		dc := newDownstreamConn(s, netConn)
		go func() {
			s.lock.Lock()
			s.downstreamConns = append(s.downstreamConns, dc)
			s.lock.Unlock()

			if err := dc.runUntilRegistered(); err != nil {
				dc.logger.Print(err)
			} else {
				if err := dc.readMessages(dc.user.downstreamIncoming); err != nil {
					dc.logger.Print(err)
				}
			}
			dc.Close()

			s.lock.Lock()
			for i := range s.downstreamConns {
				if s.downstreamConns[i] == dc {
					s.downstreamConns = append(s.downstreamConns[:i], s.downstreamConns[i+1:]...)
					break
				}
			}
			s.lock.Unlock()
		}()
	}
}
