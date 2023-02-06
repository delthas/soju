package soju

import (
	"context"
	"net"
	"os"
	"testing"

	"gopkg.in/irc.v4"

	"git.sr.ht/~emersion/soju/database"
)

var testServerPrefix = &irc.Prefix{Name: "soju-test-server"}

const (
	testUsername = "soju-test-user"
	testPassword = testUsername
)

func createTempSqliteDB(t *testing.T) database.Database {
	if !database.SqliteEnabled {
		t.Skip("SQLite support is disabled")
	}

	db, err := database.OpenTempSqliteDB()
	if err != nil {
		t.Fatalf("failed to create temporary SQLite database: %v", err)
	}
	return db
}

func createTempPostgresDB(t *testing.T) database.Database {
	source, ok := os.LookupEnv("SOJU_TEST_POSTGRES")
	if !ok {
		t.Skip("set SOJU_TEST_POSTGRES to a connection string to execute PostgreSQL tests")
	}

	db, err := database.OpenTempPostgresDB(source)
	if err != nil {
		t.Fatalf("failed to create temporary PostgreSQL database: %v", err)
	}

	return db
}

func createTestUser(t *testing.T, db database.Database) *database.User {
	record := &database.User{
		Username: testUsername,
		Enabled:  true,
	}
	if err := record.SetPassword(testPassword); err != nil {
		t.Fatalf("failed to generate bcrypt hash: %v", err)
	}
	if err := db.StoreUser(context.Background(), record); err != nil {
		t.Fatalf("failed to store test user: %v", err)
	}

	return record
}

func createTestDownstream(t *testing.T, srv *Server) ircConn {
	c1, c2 := net.Pipe()
	go srv.Handle(newNetIRCConn(c1))
	return newNetIRCConn(c2)
}

func createTestUpstream(t *testing.T, db database.Database, user *database.User) (*database.Network, net.Listener) {
	ln, err := net.Listen("tcp", "localhost:0")
	if err != nil {
		t.Fatalf("failed to create TCP listener: %v", err)
	}

	network := &database.Network{
		Name:    "testnet",
		Addr:    "irc+insecure://" + ln.Addr().String(),
		Nick:    user.Username,
		Enabled: true,
	}
	if err := db.StoreNetwork(context.Background(), user.ID, network); err != nil {
		t.Fatalf("failed to store test network: %v", err)
	}

	return network, ln
}

func mustAccept(t *testing.T, ln net.Listener) ircConn {
	c, err := ln.Accept()
	if err != nil {
		t.Fatalf("failed accepting connection: %v", err)
	}
	return newNetIRCConn(c)
}

func expectMessage(t *testing.T, c ircConn, cmd string) *irc.Message {
	msg, err := c.ReadMessage()
	if err != nil {
		t.Fatalf("failed to read IRC message (want %q): %v", cmd, err)
	}
	if msg.Command != cmd {
		t.Fatalf("invalid message received: want %q, got: %v", cmd, msg)
	}
	return msg
}

func registerDownstreamConn(t *testing.T, c ircConn, network *database.Network) {
	c.WriteMessage(&irc.Message{
		Command: "PASS",
		Params:  []string{testPassword},
	})
	c.WriteMessage(&irc.Message{
		Command: "NICK",
		Params:  []string{testUsername},
	})
	c.WriteMessage(&irc.Message{
		Command: "USER",
		Params:  []string{testUsername + "/" + network.Name, "0", "*", testUsername},
	})

	expectMessage(t, c, irc.RPL_WELCOME)
}

func registerUpstreamConn(t *testing.T, c ircConn) {
	msg := expectMessage(t, c, "CAP")
	if msg.Params[0] != "LS" {
		t.Fatalf("invalid CAP LS: got: %v", msg)
	}
	msg = expectMessage(t, c, "NICK")
	nick := msg.Params[0]
	if nick != testUsername {
		t.Fatalf("invalid NICK: want %q, got: %v", testUsername, msg)
	}
	expectMessage(t, c, "USER")

	c.WriteMessage(&irc.Message{
		Prefix:  testServerPrefix,
		Command: irc.RPL_WELCOME,
		Params:  []string{nick, "Welcome!"},
	})
	c.WriteMessage(&irc.Message{
		Prefix:  testServerPrefix,
		Command: irc.RPL_YOURHOST,
		Params:  []string{nick, "Your host is soju-test-server"},
	})
	c.WriteMessage(&irc.Message{
		Prefix:  testServerPrefix,
		Command: irc.RPL_CREATED,
		Params:  []string{nick, "Who cares when the server was created?"},
	})
	c.WriteMessage(&irc.Message{
		Prefix:  testServerPrefix,
		Command: irc.RPL_MYINFO,
		Params:  []string{nick, testServerPrefix.Name, "soju", "aiwroO", "OovaimnqpsrtklbeI"},
	})
	c.WriteMessage(&irc.Message{
		Prefix:  testServerPrefix,
		Command: irc.ERR_NOMOTD,
		Params:  []string{nick, "No MOTD"},
	})
}

func testServer(t *testing.T, db database.Database) {
	user := createTestUser(t, db)
	network, upstream := createTestUpstream(t, db, user)
	defer upstream.Close()

	srv := NewServer(db)
	if err := srv.Start(); err != nil {
		t.Fatalf("failed to start server: %v", err)
	}
	defer srv.Shutdown()

	uc := mustAccept(t, upstream)
	defer uc.Close()
	registerUpstreamConn(t, uc)

	dc := createTestDownstream(t, srv)
	defer dc.Close()
	registerDownstreamConn(t, dc, network)

	noticeText := "This is a very important server notice."
	uc.WriteMessage(&irc.Message{
		Prefix:  testServerPrefix,
		Command: "NOTICE",
		Params:  []string{testUsername, noticeText},
	})

	var msg *irc.Message
	for {
		var err error
		msg, err = dc.ReadMessage()
		if err != nil {
			t.Fatalf("failed to read IRC message: %v", err)
		}
		if msg.Command == "NOTICE" {
			break
		}
	}

	if msg.Params[1] != noticeText {
		t.Fatalf("invalid NOTICE text: want %q, got: %v", noticeText, msg)
	}
}

func TestServer(t *testing.T) {
	t.Run("sqlite", func(t *testing.T) {
		db := createTempSqliteDB(t)
		testServer(t, db)
	})

	t.Run("postgres", func(t *testing.T) {
		db := createTempPostgresDB(t)
		testServer(t, db)
	})
}
