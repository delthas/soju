package soju

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/irc.v3"
)

type messageLogger struct {
	network *network
	entity  string

	path string
	file *os.File
}

func newMessageLogger(network *network, entity string) *messageLogger {
	return &messageLogger{
		network: network,
		entity:  entity,
	}
}

func logPath(network *network, entity string, t time.Time) string {
	user := network.user
	srv := user.srv

	// TODO: handle/forbid network/entity names with illegal path characters
	year, month, day := t.Date()
	filename := fmt.Sprintf("%04d-%02d-%02d.log", year, month, day)
	return filepath.Join(srv.LogPath, user.Username, network.GetName(), entity, filename)
}

func (ml *messageLogger) Append(msg *irc.Message) error {
	s := formatMessage(msg)
	if s == "" {
		return nil
	}

	var t time.Time
	if tag, ok := msg.Tags["time"]; ok {
		var err error
		t, err = time.Parse(serverTimeLayout, string(tag))
		if err != nil {
			return fmt.Errorf("failed to parse message time tag: %v", err)
		}
		t = t.In(time.Local)
	} else {
		t = time.Now()
	}

	// TODO: enforce maximum open file handles (LRU cache of file handles)
	// TODO: handle non-monotonic clock behaviour
	path := logPath(ml.network, ml.entity, t)
	if ml.path != path {
		if ml.file != nil {
			ml.file.Close()
		}

		dir := filepath.Dir(path)
		if err := os.MkdirAll(dir, 0700); err != nil {
			return fmt.Errorf("failed to create logs directory %q: %v", dir, err)
		}

		f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_APPEND, 0600)
		if err != nil {
			return fmt.Errorf("failed to open log file %q: %v", path, err)
		}

		ml.path = path
		ml.file = f
	}

	_, err := fmt.Fprintf(ml.file, "[%02d:%02d:%02d] %s\n", t.Hour(), t.Minute(), t.Second(), s)
	if err != nil {
		return fmt.Errorf("failed to log message to %q: %v", ml.path, err)
	}
	return nil
}

func (ml *messageLogger) Close() error {
	if ml.file == nil {
		return nil
	}
	return ml.file.Close()
}

// formatMessage formats a message log line. It assumes a well-formed IRC
// message.
func formatMessage(msg *irc.Message) string {
	switch strings.ToUpper(msg.Command) {
	case "NICK":
		return fmt.Sprintf("*** %s is now known as %s", msg.Prefix.Name, msg.Params[0])
	case "JOIN":
		return fmt.Sprintf("*** Joins: %s (%s@%s)", msg.Prefix.Name, msg.Prefix.User, msg.Prefix.Host)
	case "PART":
		var reason string
		if len(msg.Params) > 1 {
			reason = msg.Params[1]
		}
		return fmt.Sprintf("*** Parts: %s (%s@%s) (%s)", msg.Prefix.Name, msg.Prefix.User, msg.Prefix.Host, reason)
	case "KICK":
		nick := msg.Params[1]
		var reason string
		if len(msg.Params) > 2 {
			reason = msg.Params[2]
		}
		return fmt.Sprintf("*** %s was kicked by %s (%s)", nick, msg.Prefix.Name, reason)
	case "QUIT":
		var reason string
		if len(msg.Params) > 0 {
			reason = msg.Params[0]
		}
		return fmt.Sprintf("*** Quits: %s (%s@%s) (%s)", msg.Prefix.Name, msg.Prefix.User, msg.Prefix.Host, reason)
	case "TOPIC":
		var topic string
		if len(msg.Params) > 1 {
			topic = msg.Params[1]
		}
		return fmt.Sprintf("*** %s changes topic to '%s'", msg.Prefix.Name, topic)
	case "MODE":
		return fmt.Sprintf("*** %s sets mode: %s", msg.Prefix.Name, strings.Join(msg.Params[1:], " "))
	case "NOTICE":
		return fmt.Sprintf("-%s- %s", msg.Prefix.Name, msg.Params[1])
	case "PRIVMSG":
		return fmt.Sprintf("<%s> %s", msg.Prefix.Name, msg.Params[1])
	default:
		return ""
	}
}

func parseMessagesBefore(network *network, entity string, timestamp time.Time, limit int) ([]*irc.Message, error) {
	year, month, day := timestamp.Date()
	path := logPath(network, entity, timestamp)
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	defer f.Close()

	historyRing := make([]*irc.Message, limit)
	cur := 0

	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		var hour, minute, second int
		_, err := fmt.Sscanf(line, "[%02d:%02d:%02d] ", &hour, &minute, &second)
		if err != nil {
			return nil, err
		}
		message := line[11:]
		// TODO: support NOTICE
		if !strings.HasPrefix(message, "<") {
			continue
		}
		i := strings.Index(message, "> ")
		if i == -1 {
			continue
		}
		t := time.Date(year, month, day, hour, minute, second, 0, time.Local)
		if !t.Before(timestamp) {
			break
		}

		sender := message[1:i]
		text := message[i+2:]
		historyRing[cur%limit] = &irc.Message{
			Tags: map[string]irc.TagValue{
				"time": irc.TagValue(t.UTC().Format(serverTimeLayout)),
			},
			Prefix: &irc.Prefix{
				Name: sender,
			},
			Command: "PRIVMSG",
			Params:  []string{entity, text},
		}
		cur++
	}
	if sc.Err() != nil {
		return nil, sc.Err()
	}

	n := limit
	if cur < limit {
		n = cur
	}
	start := (cur - n + limit) % limit

	if start+n <= limit { // ring doesnt wrap
		return historyRing[start : start+n], nil
	} else { // ring wraps
		history := make([]*irc.Message, n)
		r := copy(history, historyRing[start:])
		copy(history[r:], historyRing[:n-r])
		return history, nil
	}
}
