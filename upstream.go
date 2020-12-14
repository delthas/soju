package soju

import (
	"crypto"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/emersion/go-sasl"
	"gopkg.in/irc.v3"
)

// permanentUpstreamCaps is the static list of upstream capabilities always
// requested when supported.
var permanentUpstreamCaps = map[string]bool{
	"away-notify":      true,
	"batch":            true,
	"extended-join":    true,
	"labeled-response": true,
	"message-tags":     true,
	"multi-prefix":     true,
	"server-time":      true,
}

type registrationError string

func (err registrationError) Error() string {
	return fmt.Sprintf("registration error: %v", string(err))
}

type upstreamChannel struct {
	Name         string
	conn         *upstreamConn
	Topic        string
	TopicWho     *irc.Prefix
	TopicTime    time.Time
	Status       channelStatus
	modes        channelModes
	creationTime string
	Members      map[string]*memberships
	complete     bool
	detachTimer  *time.Timer
}

func (uc *upstreamChannel) updateAutoDetach(dur time.Duration) {
	if uc.detachTimer != nil {
		uc.detachTimer.Stop()
		uc.detachTimer = nil
	}

	if dur == 0 {
		return
	}

	uc.detachTimer = time.AfterFunc(dur, func() {
		uc.conn.network.user.events <- eventChannelDetach{
			uc:   uc.conn,
			name: uc.Name,
		}
	})
}

type upstreamConn struct {
	conn

	network *network
	user    *user

	serverName            string
	availableUserModes    string
	availableChannelModes map[byte]channelModeType
	availableChannelTypes string
	availableMemberships  []membership

	registered    bool
	nick          string
	username      string
	realname      string
	modes         userModes
	channels      map[string]*upstreamChannel
	supportedCaps map[string]string
	caps          map[string]bool
	batches       map[string]batch
	away          bool
	nextLabelID   uint64

	saslClient  sasl.Client
	saslStarted bool

	// set of LIST commands in progress, per downstream
	pendingLISTDownstreamSet map[uint64]struct{}
}

func connectToUpstream(network *network) (*upstreamConn, error) {
	logger := &prefixLogger{network.user.srv.Logger, fmt.Sprintf("upstream %q: ", network.Addr)}

	dialer := net.Dialer{Timeout: connectTimeout}

	s := network.Addr
	if !strings.Contains(s, "://") {
		// This is a raw domain name, make it an URL with the default scheme
		s = "ircs://" + s
	}

	u, err := url.Parse(s)
	if err != nil {
		return nil, fmt.Errorf("failed to parse upstream server URL: %v", err)
	}

	var netConn net.Conn
	switch u.Scheme {
	case "ircs":
		addr := u.Host
		host, _, err := net.SplitHostPort(u.Host)
		if err != nil {
			host = u.Host
			addr = u.Host + ":6697"
		}

		logger.Printf("connecting to TLS server at address %q", addr)

		tlsConfig := &tls.Config{ServerName: host}
		if network.SASL.Mechanism == "EXTERNAL" {
			if network.SASL.External.CertBlob == nil {
				return nil, fmt.Errorf("missing certificate for authentication")
			}
			if network.SASL.External.PrivKeyBlob == nil {
				return nil, fmt.Errorf("missing private key for authentication")
			}
			key, err := x509.ParsePKCS8PrivateKey(network.SASL.External.PrivKeyBlob)
			if err != nil {
				return nil, fmt.Errorf("failed to parse private key: %v", err)
			}
			tlsConfig.Certificates = []tls.Certificate{
				{
					Certificate: [][]byte{network.SASL.External.CertBlob},
					PrivateKey:  key.(crypto.PrivateKey),
				},
			}
			logger.Printf("using TLS client certificate %x", sha256.Sum256(network.SASL.External.CertBlob))
		}

		netConn, err = dialer.Dial("tcp", addr)
		if err != nil {
			return nil, fmt.Errorf("failed to dial %q: %v", addr, err)
		}

		// Don't do the TLS handshake immediately, because we need to register
		// the new connection with identd ASAP. See:
		// https://todo.sr.ht/~emersion/soju/69#event-41859
		netConn = tls.Client(netConn, tlsConfig)
	case "irc+insecure":
		addr := u.Host
		if _, _, err := net.SplitHostPort(addr); err != nil {
			addr = addr + ":6667"
		}

		logger.Printf("connecting to plain-text server at address %q", addr)
		netConn, err = dialer.Dial("tcp", addr)
		if err != nil {
			return nil, fmt.Errorf("failed to dial %q: %v", addr, err)
		}
	case "irc+unix", "unix":
		logger.Printf("connecting to Unix socket at path %q", u.Path)
		netConn, err = dialer.Dial("unix", u.Path)
		if err != nil {
			return nil, fmt.Errorf("failed to connect to Unix socket %q: %v", u.Path, err)
		}
	default:
		return nil, fmt.Errorf("failed to dial %q: unknown scheme: %v", network.Addr, u.Scheme)
	}

	options := connOptions{
		Logger:         logger,
		RateLimitDelay: upstreamMessageDelay,
		RateLimitBurst: upstreamMessageBurst,
	}

	uc := &upstreamConn{
		conn:                     *newConn(network.user.srv, newNetIRCConn(netConn), &options),
		network:                  network,
		user:                     network.user,
		channels:                 make(map[string]*upstreamChannel),
		supportedCaps:            make(map[string]string),
		caps:                     make(map[string]bool),
		batches:                  make(map[string]batch),
		availableChannelTypes:    stdChannelTypes,
		availableChannelModes:    stdChannelModes,
		availableMemberships:     stdMemberships,
		pendingLISTDownstreamSet: make(map[uint64]struct{}),
	}
	return uc, nil
}

func (uc *upstreamConn) forEachDownstream(f func(*downstreamConn)) {
	uc.network.forEachDownstream(f)
}

func (uc *upstreamConn) forEachDownstreamByID(id uint64, f func(*downstreamConn)) {
	uc.forEachDownstream(func(dc *downstreamConn) {
		if id != 0 && id != dc.id {
			return
		}
		f(dc)
	})
}

func (uc *upstreamConn) getChannel(name string) (*upstreamChannel, error) {
	ch, ok := uc.channels[name]
	if !ok {
		return nil, fmt.Errorf("unknown channel %q", name)
	}
	return ch, nil
}

func (uc *upstreamConn) isChannel(entity string) bool {
	if i := strings.IndexByte(uc.availableChannelTypes, entity[0]); i >= 0 {
		return true
	}
	return false
}

func (uc *upstreamConn) getPendingLIST() *pendingLIST {
	for _, pl := range uc.user.pendingLISTs {
		if _, ok := pl.pendingCommands[uc.network.ID]; !ok {
			continue
		}
		return &pl
	}
	return nil
}

func (uc *upstreamConn) endPendingLISTs(all bool) (found bool) {
	found = false
	for i := 0; i < len(uc.user.pendingLISTs); i++ {
		pl := uc.user.pendingLISTs[i]
		if _, ok := pl.pendingCommands[uc.network.ID]; !ok {
			continue
		}
		delete(pl.pendingCommands, uc.network.ID)
		if len(pl.pendingCommands) == 0 {
			uc.user.pendingLISTs = append(uc.user.pendingLISTs[:i], uc.user.pendingLISTs[i+1:]...)
			i--
			uc.forEachDownstreamByID(pl.downstreamID, func(dc *downstreamConn) {
				dc.SendMessage(&irc.Message{
					Prefix:  dc.srv.prefix(),
					Command: irc.RPL_LISTEND,
					Params:  []string{dc.nick, "End of /LIST"},
				})
			})
		}
		found = true
		if !all {
			delete(uc.pendingLISTDownstreamSet, pl.downstreamID)
			uc.user.forEachUpstream(func(uc *upstreamConn) {
				uc.trySendLIST(pl.downstreamID)
			})
			return
		}
	}
	return
}

func (uc *upstreamConn) trySendLIST(downstreamID uint64) {
	if _, ok := uc.pendingLISTDownstreamSet[downstreamID]; ok {
		// a LIST command is already pending
		// we will try again when that command is completed
		return
	}

	for _, pl := range uc.user.pendingLISTs {
		if pl.downstreamID != downstreamID {
			continue
		}
		// this is the first pending LIST command list of the downstream
		listCommand, ok := pl.pendingCommands[uc.network.ID]
		if !ok {
			// there is no command for this upstream in these LIST commands
			// do not send anything
			continue
		}
		// there is a command for this upstream in these LIST commands
		// send it now

		uc.SendMessageLabeled(downstreamID, listCommand)

		uc.pendingLISTDownstreamSet[downstreamID] = struct{}{}
		return
	}
}

func (uc *upstreamConn) parseMembershipPrefix(s string) (ms *memberships, nick string) {
	memberships := make(memberships, 0, 4)
	i := 0
	for _, m := range uc.availableMemberships {
		if i >= len(s) {
			break
		}
		if s[i] == m.Prefix {
			memberships = append(memberships, m)
			i++
		}
	}
	return &memberships, s[i:]
}

func isWordBoundary(r rune) bool {
	switch r {
	case '-', '_', '|':
		return false
	case '\u00A0':
		return true
	default:
		return !unicode.IsLetter(r) && !unicode.IsNumber(r)
	}
}

func isHighlight(text, nick string) bool {
	for {
		i := strings.Index(text, nick)
		if i < 0 {
			return false
		}

		// Detect word boundaries
		var left, right rune
		if i > 0 {
			left, _ = utf8.DecodeLastRuneInString(text[:i])
		}
		if i < len(text) {
			right, _ = utf8.DecodeRuneInString(text[i+len(nick):])
		}
		if isWordBoundary(left) && isWordBoundary(right) {
			return true
		}

		text = text[i+len(nick):]
	}
}

func (uc *upstreamConn) handleMessage(msg *irc.Message) error {
	var label string
	if l, ok := msg.GetTag("label"); ok {
		label = l
	}

	var msgBatch *batch
	if batchName, ok := msg.GetTag("batch"); ok {
		b, ok := uc.batches[batchName]
		if !ok {
			return fmt.Errorf("unexpected batch reference: batch was not defined: %q", batchName)
		}
		msgBatch = &b
		if label == "" {
			label = msgBatch.Label
		}
	}

	var downstreamID uint64 = 0
	if label != "" {
		var labelOffset uint64
		n, err := fmt.Sscanf(label, "sd-%d-%d", &downstreamID, &labelOffset)
		if err == nil && n < 2 {
			err = errors.New("not enough arguments")
		}
		if err != nil {
			return fmt.Errorf("unexpected message label: invalid downstream reference for label %q: %v", label, err)
		}
	}

	if _, ok := msg.Tags["time"]; !ok {
		msg.Tags["time"] = irc.TagValue(time.Now().UTC().Format(serverTimeLayout))
	}

	switch msg.Command {
	case "PING":
		uc.SendMessage(&irc.Message{
			Command: "PONG",
			Params:  msg.Params,
		})
		return nil
	case "NOTICE", "PRIVMSG", "TAGMSG":
		if msg.Prefix == nil {
			return fmt.Errorf("expected a prefix")
		}

		var entity, text string
		if msg.Command != "TAGMSG" {
			if err := parseMessageParams(msg, &entity, &text); err != nil {
				return err
			}
		} else {
			if err := parseMessageParams(msg, &entity); err != nil {
				return err
			}
		}

		if msg.Prefix.Name == serviceNick {
			uc.logger.Printf("skipping %v from soju's service: %v", msg.Command, msg)
			break
		}
		if entity == serviceNick {
			uc.logger.Printf("skipping %v to soju's service: %v", msg.Command, msg)
			break
		}

		if msg.Prefix.User == "" && msg.Prefix.Host == "" { // server message
			uc.produce("", msg, nil)
		} else { // regular user message
			target := entity
			if target == uc.nick {
				target = msg.Prefix.Name
			}

			if ch, ok := uc.network.channels[target]; ok {
				if ch.Detached {
					uc.handleDetachedMessage(msg.Prefix.Name, text, ch)
				}

				highlight := msg.Prefix.Name != uc.nick && isHighlight(text, uc.nick)
				if ch.DetachOn == FilterMessage || ch.DetachOn == FilterDefault || (ch.DetachOn == FilterHighlight && highlight) {
					uc.updateChannelAutoDetach(target)
				}
			}

			uc.produce(target, msg, nil)
		}
	case "CAP":
		var subCmd string
		if err := parseMessageParams(msg, nil, &subCmd); err != nil {
			return err
		}
		subCmd = strings.ToUpper(subCmd)
		subParams := msg.Params[2:]
		switch subCmd {
		case "LS":
			if len(subParams) < 1 {
				return newNeedMoreParamsError(msg.Command)
			}
			caps := subParams[len(subParams)-1]
			more := len(subParams) >= 2 && msg.Params[len(subParams)-2] == "*"

			uc.handleSupportedCaps(caps)

			if more {
				break // wait to receive all capabilities
			}

			uc.requestCaps()

			if uc.requestSASL() {
				break // we'll send CAP END after authentication is completed
			}

			uc.SendMessage(&irc.Message{
				Command: "CAP",
				Params:  []string{"END"},
			})
		case "ACK", "NAK":
			if len(subParams) < 1 {
				return newNeedMoreParamsError(msg.Command)
			}
			caps := strings.Fields(subParams[0])

			for _, name := range caps {
				if err := uc.handleCapAck(strings.ToLower(name), subCmd == "ACK"); err != nil {
					return err
				}
			}

			if uc.registered {
				uc.forEachDownstream(func(dc *downstreamConn) {
					dc.updateSupportedCaps()
				})
			}
		case "NEW":
			if len(subParams) < 1 {
				return newNeedMoreParamsError(msg.Command)
			}
			uc.handleSupportedCaps(subParams[0])
			uc.requestCaps()
		case "DEL":
			if len(subParams) < 1 {
				return newNeedMoreParamsError(msg.Command)
			}
			caps := strings.Fields(subParams[0])

			for _, c := range caps {
				delete(uc.supportedCaps, c)
				delete(uc.caps, c)
			}

			if uc.registered {
				uc.forEachDownstream(func(dc *downstreamConn) {
					dc.updateSupportedCaps()
				})
			}
		default:
			uc.logger.Printf("unhandled message: %v", msg)
		}
	case "AUTHENTICATE":
		if uc.saslClient == nil {
			return fmt.Errorf("received unexpected AUTHENTICATE message")
		}

		// TODO: if a challenge is 400 bytes long, buffer it
		var challengeStr string
		if err := parseMessageParams(msg, &challengeStr); err != nil {
			uc.SendMessage(&irc.Message{
				Command: "AUTHENTICATE",
				Params:  []string{"*"},
			})
			return err
		}

		var challenge []byte
		if challengeStr != "+" {
			var err error
			challenge, err = base64.StdEncoding.DecodeString(challengeStr)
			if err != nil {
				uc.SendMessage(&irc.Message{
					Command: "AUTHENTICATE",
					Params:  []string{"*"},
				})
				return err
			}
		}

		var resp []byte
		var err error
		if !uc.saslStarted {
			_, resp, err = uc.saslClient.Start()
			uc.saslStarted = true
		} else {
			resp, err = uc.saslClient.Next(challenge)
		}
		if err != nil {
			uc.SendMessage(&irc.Message{
				Command: "AUTHENTICATE",
				Params:  []string{"*"},
			})
			return err
		}

		// TODO: send response in multiple chunks if >= 400 bytes
		var respStr = "+"
		if len(resp) != 0 {
			respStr = base64.StdEncoding.EncodeToString(resp)
		}

		uc.SendMessage(&irc.Message{
			Command: "AUTHENTICATE",
			Params:  []string{respStr},
		})
	case irc.RPL_LOGGEDIN:
		var account string
		if err := parseMessageParams(msg, nil, nil, &account); err != nil {
			return err
		}
		uc.logger.Printf("logged in with account %q", account)
	case irc.RPL_LOGGEDOUT:
		uc.logger.Printf("logged out")
	case irc.ERR_NICKLOCKED, irc.RPL_SASLSUCCESS, irc.ERR_SASLFAIL, irc.ERR_SASLTOOLONG, irc.ERR_SASLABORTED:
		var info string
		if err := parseMessageParams(msg, nil, &info); err != nil {
			return err
		}
		switch msg.Command {
		case irc.ERR_NICKLOCKED:
			uc.logger.Printf("invalid nick used with SASL authentication: %v", info)
		case irc.ERR_SASLFAIL:
			uc.logger.Printf("SASL authentication failed: %v", info)
		case irc.ERR_SASLTOOLONG:
			uc.logger.Printf("SASL message too long: %v", info)
		}

		uc.saslClient = nil
		uc.saslStarted = false

		uc.SendMessage(&irc.Message{
			Command: "CAP",
			Params:  []string{"END"},
		})
	case irc.RPL_WELCOME:
		uc.registered = true
		uc.logger.Printf("connection registered")

		uc.forEachDownstream(func(dc *downstreamConn) {
			dc.updateSupportedCaps()
		})

		if len(uc.network.channels) > 0 {
			var channels, keys []string
			for _, ch := range uc.network.channels {
				channels = append(channels, ch.Name)
				keys = append(keys, ch.Key)
			}

			for _, msg := range join(channels, keys) {
				uc.SendMessage(msg)
			}
		}
	case irc.RPL_MYINFO:
		if err := parseMessageParams(msg, nil, &uc.serverName, nil, &uc.availableUserModes, nil); err != nil {
			return err
		}
	case irc.RPL_ISUPPORT:
		if err := parseMessageParams(msg, nil, nil); err != nil {
			return err
		}
		for _, token := range msg.Params[1 : len(msg.Params)-1] {
			negate := false
			parameter := token
			value := ""
			if strings.HasPrefix(token, "-") {
				negate = true
				token = token[1:]
			} else {
				if i := strings.IndexByte(token, '='); i >= 0 {
					parameter = token[:i]
					value = token[i+1:]
				}
			}
			if !negate {
				switch parameter {
				case "CHANMODES":
					parts := strings.SplitN(value, ",", 5)
					if len(parts) < 4 {
						return fmt.Errorf("malformed ISUPPORT CHANMODES value: %v", value)
					}
					modes := make(map[byte]channelModeType)
					for i, mt := range []channelModeType{modeTypeA, modeTypeB, modeTypeC, modeTypeD} {
						for j := 0; j < len(parts[i]); j++ {
							mode := parts[i][j]
							modes[mode] = mt
						}
					}
					uc.availableChannelModes = modes
				case "CHANTYPES":
					uc.availableChannelTypes = value
				case "PREFIX":
					if value == "" {
						uc.availableMemberships = nil
					} else {
						if value[0] != '(' {
							return fmt.Errorf("malformed ISUPPORT PREFIX value: %v", value)
						}
						sep := strings.IndexByte(value, ')')
						if sep < 0 || len(value) != sep*2 {
							return fmt.Errorf("malformed ISUPPORT PREFIX value: %v", value)
						}
						memberships := make([]membership, len(value)/2-1)
						for i := range memberships {
							memberships[i] = membership{
								Mode:   value[i+1],
								Prefix: value[sep+i+1],
							}
						}
						uc.availableMemberships = memberships
					}
				}
			} else {
				// TODO: handle ISUPPORT negations
			}
		}
	case "BATCH":
		var tag string
		if err := parseMessageParams(msg, &tag); err != nil {
			return err
		}

		if strings.HasPrefix(tag, "+") {
			tag = tag[1:]
			if _, ok := uc.batches[tag]; ok {
				return fmt.Errorf("unexpected BATCH reference tag: batch was already defined: %q", tag)
			}
			var batchType string
			if err := parseMessageParams(msg, nil, &batchType); err != nil {
				return err
			}
			label := label
			if label == "" && msgBatch != nil {
				label = msgBatch.Label
			}
			uc.batches[tag] = batch{
				Type:   batchType,
				Params: msg.Params[2:],
				Outer:  msgBatch,
				Label:  label,
			}
		} else if strings.HasPrefix(tag, "-") {
			tag = tag[1:]
			if _, ok := uc.batches[tag]; !ok {
				return fmt.Errorf("unknown BATCH reference tag: %q", tag)
			}
			delete(uc.batches, tag)
		} else {
			return fmt.Errorf("unexpected BATCH reference tag: missing +/- prefix: %q", tag)
		}
	case "NICK":
		if msg.Prefix == nil {
			return fmt.Errorf("expected a prefix")
		}

		var newNick string
		if err := parseMessageParams(msg, &newNick); err != nil {
			return err
		}

		me := false
		if msg.Prefix.Name == uc.nick {
			uc.logger.Printf("changed nick from %q to %q", uc.nick, newNick)
			me = true
			uc.nick = newNick
		}

		for _, ch := range uc.channels {
			if memberships, ok := ch.Members[msg.Prefix.Name]; ok {
				delete(ch.Members, msg.Prefix.Name)
				ch.Members[newNick] = memberships
				uc.appendLog(ch.Name, msg)
			}
		}

		if !me {
			uc.forEachDownstream(func(dc *downstreamConn) {
				dc.SendMessage(dc.marshalMessage(msg, uc.network))
			})
		} else {
			uc.forEachDownstream(func(dc *downstreamConn) {
				dc.updateNick()
			})
		}
	case "JOIN":
		if msg.Prefix == nil {
			return fmt.Errorf("expected a prefix")
		}

		var channels string
		if err := parseMessageParams(msg, &channels); err != nil {
			return err
		}

		for _, ch := range strings.Split(channels, ",") {
			if msg.Prefix.Name == uc.nick {
				uc.logger.Printf("joined channel %q", ch)
				uc.channels[ch] = &upstreamChannel{
					Name:    ch,
					conn:    uc,
					Members: make(map[string]*memberships),
				}
				uc.updateChannelAutoDetach(ch)

				uc.SendMessage(&irc.Message{
					Command: "MODE",
					Params:  []string{ch},
				})
			} else {
				ch, err := uc.getChannel(ch)
				if err != nil {
					return err
				}
				ch.Members[msg.Prefix.Name] = &memberships{}
			}

			chMsg := msg.Copy()
			chMsg.Params[0] = ch
			uc.produce(ch, chMsg, nil)
		}
	case "PART":
		if msg.Prefix == nil {
			return fmt.Errorf("expected a prefix")
		}

		var channels string
		if err := parseMessageParams(msg, &channels); err != nil {
			return err
		}

		for _, ch := range strings.Split(channels, ",") {
			if msg.Prefix.Name == uc.nick {
				uc.logger.Printf("parted channel %q", ch)
				if uch, ok := uc.channels[ch]; ok {
					delete(uc.channels, ch)
					uch.updateAutoDetach(0)
				}
			} else {
				ch, err := uc.getChannel(ch)
				if err != nil {
					return err
				}
				delete(ch.Members, msg.Prefix.Name)
			}

			chMsg := msg.Copy()
			chMsg.Params[0] = ch
			uc.produce(ch, chMsg, nil)
		}
	case "KICK":
		if msg.Prefix == nil {
			return fmt.Errorf("expected a prefix")
		}

		var channel, user string
		if err := parseMessageParams(msg, &channel, &user); err != nil {
			return err
		}

		if user == uc.nick {
			uc.logger.Printf("kicked from channel %q by %s", channel, msg.Prefix.Name)
			delete(uc.channels, channel)
		} else {
			ch, err := uc.getChannel(channel)
			if err != nil {
				return err
			}
			delete(ch.Members, user)
		}

		uc.produce(channel, msg, nil)
	case "QUIT":
		if msg.Prefix == nil {
			return fmt.Errorf("expected a prefix")
		}

		if msg.Prefix.Name == uc.nick {
			uc.logger.Printf("quit")
		}

		for _, ch := range uc.channels {
			if _, ok := ch.Members[msg.Prefix.Name]; ok {
				delete(ch.Members, msg.Prefix.Name)

				uc.appendLog(ch.Name, msg)
			}
		}

		if msg.Prefix.Name != uc.nick {
			uc.forEachDownstream(func(dc *downstreamConn) {
				dc.SendMessage(dc.marshalMessage(msg, uc.network))
			})
		}
	case irc.RPL_TOPIC, irc.RPL_NOTOPIC:
		var name, topic string
		if err := parseMessageParams(msg, nil, &name, &topic); err != nil {
			return err
		}
		ch, err := uc.getChannel(name)
		if err != nil {
			return err
		}
		if msg.Command == irc.RPL_TOPIC {
			ch.Topic = topic
		} else {
			ch.Topic = ""
		}
	case "TOPIC":
		if msg.Prefix == nil {
			return fmt.Errorf("expected a prefix")
		}

		var name string
		if err := parseMessageParams(msg, &name); err != nil {
			return err
		}
		ch, err := uc.getChannel(name)
		if err != nil {
			return err
		}
		if len(msg.Params) > 1 {
			ch.Topic = msg.Params[1]
			ch.TopicWho = msg.Prefix.Copy()
			ch.TopicTime = time.Now() // TODO use msg.Tags["time"]
		} else {
			ch.Topic = ""
		}
		uc.produce(ch.Name, msg, nil)
	case "MODE":
		var name, modeStr string
		if err := parseMessageParams(msg, &name, &modeStr); err != nil {
			return err
		}

		if !uc.isChannel(name) { // user mode change
			if name != uc.nick {
				return fmt.Errorf("received MODE message for unknown nick %q", name)
			}
			return uc.modes.Apply(modeStr)
			// TODO: notify downstreams about user mode change?
		} else { // channel mode change
			ch, err := uc.getChannel(name)
			if err != nil {
				return err
			}

			needMarshaling, err := applyChannelModes(ch, modeStr, msg.Params[2:])
			if err != nil {
				return err
			}

			uc.appendLog(ch.Name, msg)

			if ch, ok := uc.network.channels[name]; !ok || !ch.Detached {
				uc.forEachDownstream(func(dc *downstreamConn) {
					params := make([]string, len(msg.Params))
					params[0] = dc.marshalEntity(uc.network, name)
					params[1] = modeStr

					copy(params[2:], msg.Params[2:])
					for i, modeParam := range params[2:] {
						if _, ok := needMarshaling[i]; ok {
							params[2+i] = dc.marshalEntity(uc.network, modeParam)
						}
					}

					dc.SendMessage(&irc.Message{
						Prefix:  dc.marshalUserPrefix(uc.network, msg.Prefix),
						Command: "MODE",
						Params:  params,
					})
				})
			}
		}
	case irc.RPL_UMODEIS:
		if err := parseMessageParams(msg, nil); err != nil {
			return err
		}
		modeStr := ""
		if len(msg.Params) > 1 {
			modeStr = msg.Params[1]
		}

		uc.modes = ""
		if err := uc.modes.Apply(modeStr); err != nil {
			return err
		}
		// TODO: send RPL_UMODEIS to downstream connections when applicable
	case irc.RPL_CHANNELMODEIS:
		var channel string
		if err := parseMessageParams(msg, nil, &channel); err != nil {
			return err
		}
		modeStr := ""
		if len(msg.Params) > 2 {
			modeStr = msg.Params[2]
		}

		ch, err := uc.getChannel(channel)
		if err != nil {
			return err
		}

		firstMode := ch.modes == nil
		ch.modes = make(map[byte]string)
		if _, err := applyChannelModes(ch, modeStr, msg.Params[3:]); err != nil {
			return err
		}
		if firstMode {
			if c, ok := uc.network.channels[channel]; !ok || !c.Detached {
				modeStr, modeParams := ch.modes.Format()

				uc.forEachDownstream(func(dc *downstreamConn) {
					params := []string{dc.nick, dc.marshalEntity(uc.network, channel), modeStr}
					params = append(params, modeParams...)

					dc.SendMessage(&irc.Message{
						Prefix:  dc.srv.prefix(),
						Command: irc.RPL_CHANNELMODEIS,
						Params:  params,
					})
				})
			}
		}
	case rpl_creationtime:
		var channel, creationTime string
		if err := parseMessageParams(msg, nil, &channel, &creationTime); err != nil {
			return err
		}

		ch, err := uc.getChannel(channel)
		if err != nil {
			return err
		}

		firstCreationTime := ch.creationTime == ""
		ch.creationTime = creationTime
		if firstCreationTime {
			uc.forEachDownstream(func(dc *downstreamConn) {
				dc.SendMessage(&irc.Message{
					Prefix:  dc.srv.prefix(),
					Command: rpl_creationtime,
					Params:  []string{dc.nick, dc.marshalEntity(uc.network, ch.Name), creationTime},
				})
			})
		}
	case rpl_topicwhotime:
		var name, who, timeStr string
		if err := parseMessageParams(msg, nil, &name, &who, &timeStr); err != nil {
			return err
		}
		ch, err := uc.getChannel(name)
		if err != nil {
			return err
		}
		firstTopicWhoTime := ch.TopicWho == nil
		ch.TopicWho = irc.ParsePrefix(who)
		sec, err := strconv.ParseInt(timeStr, 10, 64)
		if err != nil {
			return fmt.Errorf("failed to parse topic time: %v", err)
		}
		ch.TopicTime = time.Unix(sec, 0)
		if firstTopicWhoTime {
			uc.forEachDownstream(func(dc *downstreamConn) {
				topicWho := dc.marshalUserPrefix(uc.network, ch.TopicWho)
				dc.SendMessage(&irc.Message{
					Prefix:  dc.srv.prefix(),
					Command: rpl_topicwhotime,
					Params: []string{
						dc.nick,
						dc.marshalEntity(uc.network, ch.Name),
						topicWho.String(),
						timeStr,
					},
				})
			})
		}
	case irc.RPL_LIST:
		var channel, clients, topic string
		if err := parseMessageParams(msg, nil, &channel, &clients, &topic); err != nil {
			return err
		}

		pl := uc.getPendingLIST()
		if pl == nil {
			return fmt.Errorf("unexpected RPL_LIST: no matching pending LIST")
		}

		uc.forEachDownstreamByID(pl.downstreamID, func(dc *downstreamConn) {
			dc.SendMessage(&irc.Message{
				Prefix:  dc.srv.prefix(),
				Command: irc.RPL_LIST,
				Params:  []string{dc.nick, dc.marshalEntity(uc.network, channel), clients, topic},
			})
		})
	case irc.RPL_LISTEND:
		ok := uc.endPendingLISTs(false)
		if !ok {
			return fmt.Errorf("unexpected RPL_LISTEND: no matching pending LIST")
		}
	case irc.RPL_NAMREPLY:
		var name, statusStr, members string
		if err := parseMessageParams(msg, nil, &statusStr, &name, &members); err != nil {
			return err
		}

		ch, ok := uc.channels[name]
		if !ok {
			// NAMES on a channel we have not joined, forward to downstream
			uc.forEachDownstreamByID(downstreamID, func(dc *downstreamConn) {
				channel := dc.marshalEntity(uc.network, name)
				members := splitSpace(members)
				for i, member := range members {
					memberships, nick := uc.parseMembershipPrefix(member)
					members[i] = memberships.Format(dc) + dc.marshalEntity(uc.network, nick)
				}
				memberStr := strings.Join(members, " ")

				dc.SendMessage(&irc.Message{
					Prefix:  dc.srv.prefix(),
					Command: irc.RPL_NAMREPLY,
					Params:  []string{dc.nick, statusStr, channel, memberStr},
				})
			})
			return nil
		}

		status, err := parseChannelStatus(statusStr)
		if err != nil {
			return err
		}
		ch.Status = status

		for _, s := range splitSpace(members) {
			memberships, nick := uc.parseMembershipPrefix(s)
			ch.Members[nick] = memberships
		}
	case irc.RPL_ENDOFNAMES:
		var name string
		if err := parseMessageParams(msg, nil, &name); err != nil {
			return err
		}

		ch, ok := uc.channels[name]
		if !ok {
			// NAMES on a channel we have not joined, forward to downstream
			uc.forEachDownstreamByID(downstreamID, func(dc *downstreamConn) {
				channel := dc.marshalEntity(uc.network, name)

				dc.SendMessage(&irc.Message{
					Prefix:  dc.srv.prefix(),
					Command: irc.RPL_ENDOFNAMES,
					Params:  []string{dc.nick, channel, "End of /NAMES list"},
				})
			})
			return nil
		}

		if ch.complete {
			return fmt.Errorf("received unexpected RPL_ENDOFNAMES")
		}
		ch.complete = true

		if c, ok := uc.network.channels[name]; !ok || !c.Detached {
			uc.forEachDownstream(func(dc *downstreamConn) {
				forwardChannel(dc, ch)
			})
		}
	case irc.RPL_WHOREPLY:
		var channel, username, host, server, nick, mode, trailing string
		if err := parseMessageParams(msg, nil, &channel, &username, &host, &server, &nick, &mode, &trailing); err != nil {
			return err
		}

		parts := strings.SplitN(trailing, " ", 2)
		if len(parts) != 2 {
			return fmt.Errorf("received malformed RPL_WHOREPLY: wrong trailing parameter: %s", trailing)
		}
		realname := parts[1]
		hops, err := strconv.Atoi(parts[0])
		if err != nil {
			return fmt.Errorf("received malformed RPL_WHOREPLY: wrong hop count: %s", parts[0])
		}
		hops++

		trailing = strconv.Itoa(hops) + " " + realname

		uc.forEachDownstreamByID(downstreamID, func(dc *downstreamConn) {
			channel := channel
			if channel != "*" {
				channel = dc.marshalEntity(uc.network, channel)
			}
			nick := dc.marshalEntity(uc.network, nick)
			dc.SendMessage(&irc.Message{
				Prefix:  dc.srv.prefix(),
				Command: irc.RPL_WHOREPLY,
				Params:  []string{dc.nick, channel, username, host, server, nick, mode, trailing},
			})
		})
	case irc.RPL_ENDOFWHO:
		var name string
		if err := parseMessageParams(msg, nil, &name); err != nil {
			return err
		}

		uc.forEachDownstreamByID(downstreamID, func(dc *downstreamConn) {
			name := name
			if name != "*" {
				// TODO: support WHO masks
				name = dc.marshalEntity(uc.network, name)
			}
			dc.SendMessage(&irc.Message{
				Prefix:  dc.srv.prefix(),
				Command: irc.RPL_ENDOFWHO,
				Params:  []string{dc.nick, name, "End of /WHO list"},
			})
		})
	case irc.RPL_WHOISUSER:
		var nick, username, host, realname string
		if err := parseMessageParams(msg, nil, &nick, &username, &host, nil, &realname); err != nil {
			return err
		}

		uc.forEachDownstreamByID(downstreamID, func(dc *downstreamConn) {
			nick := dc.marshalEntity(uc.network, nick)
			dc.SendMessage(&irc.Message{
				Prefix:  dc.srv.prefix(),
				Command: irc.RPL_WHOISUSER,
				Params:  []string{dc.nick, nick, username, host, "*", realname},
			})
		})
	case irc.RPL_WHOISSERVER:
		var nick, server, serverInfo string
		if err := parseMessageParams(msg, nil, &nick, &server, &serverInfo); err != nil {
			return err
		}

		uc.forEachDownstreamByID(downstreamID, func(dc *downstreamConn) {
			nick := dc.marshalEntity(uc.network, nick)
			dc.SendMessage(&irc.Message{
				Prefix:  dc.srv.prefix(),
				Command: irc.RPL_WHOISSERVER,
				Params:  []string{dc.nick, nick, server, serverInfo},
			})
		})
	case irc.RPL_WHOISOPERATOR:
		var nick string
		if err := parseMessageParams(msg, nil, &nick); err != nil {
			return err
		}

		uc.forEachDownstreamByID(downstreamID, func(dc *downstreamConn) {
			nick := dc.marshalEntity(uc.network, nick)
			dc.SendMessage(&irc.Message{
				Prefix:  dc.srv.prefix(),
				Command: irc.RPL_WHOISOPERATOR,
				Params:  []string{dc.nick, nick, "is an IRC operator"},
			})
		})
	case irc.RPL_WHOISIDLE:
		var nick string
		if err := parseMessageParams(msg, nil, &nick, nil); err != nil {
			return err
		}

		uc.forEachDownstreamByID(downstreamID, func(dc *downstreamConn) {
			nick := dc.marshalEntity(uc.network, nick)
			params := []string{dc.nick, nick}
			params = append(params, msg.Params[2:]...)
			dc.SendMessage(&irc.Message{
				Prefix:  dc.srv.prefix(),
				Command: irc.RPL_WHOISIDLE,
				Params:  params,
			})
		})
	case irc.RPL_WHOISCHANNELS:
		var nick, channelList string
		if err := parseMessageParams(msg, nil, &nick, &channelList); err != nil {
			return err
		}
		channels := splitSpace(channelList)

		uc.forEachDownstreamByID(downstreamID, func(dc *downstreamConn) {
			nick := dc.marshalEntity(uc.network, nick)
			channelList := make([]string, len(channels))
			for i, channel := range channels {
				prefix, channel := uc.parseMembershipPrefix(channel)
				channel = dc.marshalEntity(uc.network, channel)
				channelList[i] = prefix.Format(dc) + channel
			}
			channels := strings.Join(channelList, " ")
			dc.SendMessage(&irc.Message{
				Prefix:  dc.srv.prefix(),
				Command: irc.RPL_WHOISCHANNELS,
				Params:  []string{dc.nick, nick, channels},
			})
		})
	case irc.RPL_ENDOFWHOIS:
		var nick string
		if err := parseMessageParams(msg, nil, &nick); err != nil {
			return err
		}

		uc.forEachDownstreamByID(downstreamID, func(dc *downstreamConn) {
			nick := dc.marshalEntity(uc.network, nick)
			dc.SendMessage(&irc.Message{
				Prefix:  dc.srv.prefix(),
				Command: irc.RPL_ENDOFWHOIS,
				Params:  []string{dc.nick, nick, "End of /WHOIS list"},
			})
		})
	case "INVITE":
		var nick, channel string
		if err := parseMessageParams(msg, &nick, &channel); err != nil {
			return err
		}

		uc.forEachDownstream(func(dc *downstreamConn) {
			dc.SendMessage(&irc.Message{
				Prefix:  dc.marshalUserPrefix(uc.network, msg.Prefix),
				Command: "INVITE",
				Params:  []string{dc.marshalEntity(uc.network, nick), dc.marshalEntity(uc.network, channel)},
			})
		})
	case irc.RPL_INVITING:
		var nick, channel string
		if err := parseMessageParams(msg, nil, &nick, &channel); err != nil {
			return err
		}

		uc.forEachDownstreamByID(downstreamID, func(dc *downstreamConn) {
			dc.SendMessage(&irc.Message{
				Prefix:  dc.srv.prefix(),
				Command: irc.RPL_INVITING,
				Params:  []string{dc.nick, dc.marshalEntity(uc.network, nick), dc.marshalEntity(uc.network, channel)},
			})
		})
	case irc.RPL_AWAY:
		var nick, reason string
		if err := parseMessageParams(msg, nil, &nick, &reason); err != nil {
			return err
		}

		uc.forEachDownstream(func(dc *downstreamConn) {
			dc.SendMessage(&irc.Message{
				Prefix:  dc.srv.prefix(),
				Command: irc.RPL_AWAY,
				Params:  []string{dc.nick, dc.marshalEntity(uc.network, nick), reason},
			})
		})
	case "AWAY":
		if msg.Prefix == nil {
			return fmt.Errorf("expected a prefix")
		}

		uc.forEachDownstream(func(dc *downstreamConn) {
			if !dc.caps["away-notify"] {
				return
			}
			dc.SendMessage(&irc.Message{
				Prefix:  dc.marshalUserPrefix(uc.network, msg.Prefix),
				Command: "AWAY",
				Params:  msg.Params,
			})
		})
	case irc.RPL_BANLIST, irc.RPL_INVITELIST, irc.RPL_EXCEPTLIST:
		var channel, mask string
		if err := parseMessageParams(msg, nil, &channel, &mask); err != nil {
			return err
		}
		var addNick, addTime string
		if len(msg.Params) >= 5 {
			addNick = msg.Params[3]
			addTime = msg.Params[4]
		}

		uc.forEachDownstreamByID(downstreamID, func(dc *downstreamConn) {
			channel := dc.marshalEntity(uc.network, channel)

			var params []string
			if addNick != "" && addTime != "" {
				addNick := dc.marshalEntity(uc.network, addNick)
				params = []string{dc.nick, channel, mask, addNick, addTime}
			} else {
				params = []string{dc.nick, channel, mask}
			}

			dc.SendMessage(&irc.Message{
				Prefix:  dc.srv.prefix(),
				Command: msg.Command,
				Params:  params,
			})
		})
	case irc.RPL_ENDOFBANLIST, irc.RPL_ENDOFINVITELIST, irc.RPL_ENDOFEXCEPTLIST:
		var channel, trailing string
		if err := parseMessageParams(msg, nil, &channel, &trailing); err != nil {
			return err
		}

		uc.forEachDownstreamByID(downstreamID, func(dc *downstreamConn) {
			upstreamChannel := dc.marshalEntity(uc.network, channel)
			dc.SendMessage(&irc.Message{
				Prefix:  dc.srv.prefix(),
				Command: msg.Command,
				Params:  []string{dc.nick, upstreamChannel, trailing},
			})
		})
	case irc.ERR_UNKNOWNCOMMAND, irc.RPL_TRYAGAIN:
		var command, reason string
		if err := parseMessageParams(msg, nil, &command, &reason); err != nil {
			return err
		}

		if command == "LIST" {
			ok := uc.endPendingLISTs(false)
			if !ok {
				return fmt.Errorf("unexpected response for LIST: %q: no matching pending LIST", msg.Command)
			}
		}

		uc.forEachDownstreamByID(downstreamID, func(dc *downstreamConn) {
			dc.SendMessage(&irc.Message{
				Prefix:  uc.srv.prefix(),
				Command: msg.Command,
				Params:  []string{dc.nick, command, reason},
			})
		})
	case "ACK":
		// Ignore
	case irc.RPL_NOWAWAY, irc.RPL_UNAWAY:
		// Ignore
	case irc.RPL_YOURHOST, irc.RPL_CREATED:
		// Ignore
	case irc.RPL_LUSERCLIENT, irc.RPL_LUSEROP, irc.RPL_LUSERUNKNOWN, irc.RPL_LUSERCHANNELS, irc.RPL_LUSERME:
		// Ignore
	case irc.RPL_MOTDSTART, irc.RPL_MOTD, irc.RPL_ENDOFMOTD:
		// Ignore
	case irc.RPL_LISTSTART:
		// Ignore
	case rpl_localusers, rpl_globalusers:
		// Ignore
	case irc.RPL_STATSVLINE, rpl_statsping, irc.RPL_STATSBLINE, irc.RPL_STATSDLINE:
		// Ignore
	case "ERROR":
		var text string
		if err := parseMessageParams(msg, &text); err != nil {
			return err
		}
		return fmt.Errorf("fatal server error: %v", text)
	case irc.ERR_PASSWDMISMATCH, irc.ERR_ERRONEUSNICKNAME, irc.ERR_NICKNAMEINUSE, irc.ERR_NICKCOLLISION, irc.ERR_UNAVAILRESOURCE, irc.ERR_NOPERMFORHOST, irc.ERR_YOUREBANNEDCREEP:
		if !uc.registered {
			text := msg.Params[len(msg.Params)-1]
			return registrationError(text)
		}
		fallthrough
	default:
		uc.logger.Printf("unhandled message: %v", msg)

		uc.forEachDownstreamByID(downstreamID, func(dc *downstreamConn) {
			// best effort marshaling for unknown messages, replies and errors:
			// most numerics start with the user nick, marshal it if that's the case
			// otherwise, conservately keep the params without marshaling
			params := msg.Params
			if _, err := strconv.Atoi(msg.Command); err == nil { // numeric
				if len(msg.Params) > 0 && isOurNick(uc.network, msg.Params[0]) {
					params[0] = dc.nick
				}
			}
			dc.SendMessage(&irc.Message{
				Prefix:  uc.srv.prefix(),
				Command: msg.Command,
				Params:  params,
			})
		})
	}
	return nil
}

func (uc *upstreamConn) handleDetachedMessage(sender string, text string, ch *Channel) {
	highlight := sender != uc.nick && isHighlight(text, uc.nick)
	if ch.RelayDetached == FilterMessage || ((ch.RelayDetached == FilterHighlight || ch.RelayDetached == FilterDefault) && highlight) {
		uc.forEachDownstream(func(dc *downstreamConn) {
			if highlight {
				sendServiceNOTICE(dc, fmt.Sprintf("highlight in %v: <%v> %v", dc.marshalEntity(uc.network, ch.Name), sender, text))
			} else {
				sendServiceNOTICE(dc, fmt.Sprintf("message in %v: <%v> %v", dc.marshalEntity(uc.network, ch.Name), sender, text))
			}
		})
	}
	if ch.ReattachOn == FilterMessage || (ch.ReattachOn == FilterHighlight && highlight) {
		uc.network.attach(ch)
		if err := uc.srv.db.StoreChannel(uc.network.ID, ch); err != nil {
			uc.logger.Printf("failed to update channel %q: %v", ch.Name, err)
		}
	}
}

func (uc *upstreamConn) handleSupportedCaps(capsStr string) {
	caps := strings.Fields(capsStr)
	for _, s := range caps {
		kv := strings.SplitN(s, "=", 2)
		k := strings.ToLower(kv[0])
		var v string
		if len(kv) == 2 {
			v = kv[1]
		}
		uc.supportedCaps[k] = v
	}
}

func (uc *upstreamConn) requestCaps() {
	var requestCaps []string
	for c := range permanentUpstreamCaps {
		if _, ok := uc.supportedCaps[c]; ok && !uc.caps[c] {
			requestCaps = append(requestCaps, c)
		}
	}

	if uc.requestSASL() && !uc.caps["sasl"] {
		requestCaps = append(requestCaps, "sasl")
	}

	if len(requestCaps) == 0 {
		return
	}

	uc.SendMessage(&irc.Message{
		Command: "CAP",
		Params:  []string{"REQ", strings.Join(requestCaps, " ")},
	})
}

func (uc *upstreamConn) requestSASL() bool {
	if uc.network.SASL.Mechanism == "" {
		return false
	}

	v, ok := uc.supportedCaps["sasl"]
	if !ok {
		return false
	}
	if v != "" {
		mechanisms := strings.Split(v, ",")
		found := false
		for _, mech := range mechanisms {
			if strings.EqualFold(mech, uc.network.SASL.Mechanism) {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}

	return true
}

func (uc *upstreamConn) handleCapAck(name string, ok bool) error {
	uc.caps[name] = ok

	switch name {
	case "sasl":
		if !ok {
			uc.logger.Printf("server refused to acknowledge the SASL capability")
			return nil
		}

		auth := &uc.network.SASL
		switch auth.Mechanism {
		case "PLAIN":
			uc.logger.Printf("starting SASL PLAIN authentication with username %q", auth.Plain.Username)
			uc.saslClient = sasl.NewPlainClient("", auth.Plain.Username, auth.Plain.Password)
		case "EXTERNAL":
			uc.logger.Printf("starting SASL EXTERNAL authentication")
			uc.saslClient = sasl.NewExternalClient("")
		default:
			return fmt.Errorf("unsupported SASL mechanism %q", name)
		}

		uc.SendMessage(&irc.Message{
			Command: "AUTHENTICATE",
			Params:  []string{auth.Mechanism},
		})
	default:
		if permanentUpstreamCaps[name] {
			break
		}
		uc.logger.Printf("received CAP ACK/NAK for a cap we don't support: %v", name)
	}
	return nil
}

func splitSpace(s string) []string {
	return strings.FieldsFunc(s, func(r rune) bool {
		return r == ' '
	})
}

func (uc *upstreamConn) register() {
	uc.nick = uc.network.Nick
	uc.username = uc.network.Username
	if uc.username == "" {
		uc.username = uc.nick
	}
	uc.realname = uc.network.Realname
	if uc.realname == "" {
		uc.realname = uc.nick
	}

	uc.SendMessage(&irc.Message{
		Command: "CAP",
		Params:  []string{"LS", "302"},
	})

	if uc.network.Pass != "" {
		uc.SendMessage(&irc.Message{
			Command: "PASS",
			Params:  []string{uc.network.Pass},
		})
	}

	uc.SendMessage(&irc.Message{
		Command: "NICK",
		Params:  []string{uc.nick},
	})
	uc.SendMessage(&irc.Message{
		Command: "USER",
		Params:  []string{uc.username, "0", "*", uc.realname},
	})
}

func (uc *upstreamConn) runUntilRegistered() error {
	for !uc.registered {
		msg, err := uc.ReadMessage()
		if err != nil {
			return fmt.Errorf("failed to read message: %v", err)
		}

		if err := uc.handleMessage(msg); err != nil {
			if _, ok := err.(registrationError); ok {
				return err
			} else {
				msg.Tags = nil // prevent message tags from cluttering logs
				return fmt.Errorf("failed to handle message %q: %v", msg, err)
			}
		}
	}

	for _, command := range uc.network.ConnectCommands {
		m, err := irc.ParseMessage(command)
		if err != nil {
			uc.logger.Printf("failed to parse connect command %q: %v", command, err)
		} else {
			uc.SendMessage(m)
		}
	}

	return nil
}

func (uc *upstreamConn) readMessages(ch chan<- event) error {
	for {
		msg, err := uc.ReadMessage()
		if err == io.EOF {
			break
		} else if err != nil {
			return fmt.Errorf("failed to read IRC command: %v", err)
		}

		ch <- eventUpstreamMessage{msg, uc}
	}

	return nil
}

func (uc *upstreamConn) SendMessage(msg *irc.Message) {
	if !uc.caps["message-tags"] {
		msg = msg.Copy()
		msg.Tags = nil
	}

	uc.conn.SendMessage(msg)
}

func (uc *upstreamConn) SendMessageLabeled(downstreamID uint64, msg *irc.Message) {
	if uc.caps["labeled-response"] {
		if msg.Tags == nil {
			msg.Tags = make(map[string]irc.TagValue)
		}
		msg.Tags["label"] = irc.TagValue(fmt.Sprintf("sd-%d-%d", downstreamID, uc.nextLabelID))
		uc.nextLabelID++
	}
	uc.SendMessage(msg)
}

// appendLog appends a message to the log file.
//
// The internal message ID is returned. If the message isn't recorded in the
// log file, an empty string is returned.
func (uc *upstreamConn) appendLog(entity string, msg *irc.Message) (msgID string) {
	if uc.user.msgStore == nil {
		return ""
	}

	detached := false
	if ch, ok := uc.network.channels[entity]; ok {
		detached = ch.Detached
	}

	history, ok := uc.network.history[entity]
	if !ok {
		lastID, err := uc.user.msgStore.LastMsgID(uc.network, entity, time.Now())
		if err != nil {
			uc.logger.Printf("failed to log message: failed to get last message ID: %v", err)
			return ""
		}

		history = &networkHistory{
			clients: make(map[string]string),
		}
		uc.network.history[entity] = history

		for clientName, _ := range uc.network.offlineClients {
			history.clients[clientName] = lastID
		}

		if detached {
			// If the channel is detached, online clients act as offline
			// clients too
			uc.forEachDownstream(func(dc *downstreamConn) {
				history.clients[dc.clientName] = lastID
			})
		}
	}

	msgID, err := uc.user.msgStore.Append(uc.network, entity, msg)
	if err != nil {
		uc.logger.Printf("failed to log message: %v", err)
		return ""
	}

	return msgID
}

// produce appends a message to the logs and forwards it to connected downstream
// connections.
//
// If origin is not nil and origin doesn't support echo-message, the message is
// forwarded to all connections except origin.
func (uc *upstreamConn) produce(target string, msg *irc.Message, origin *downstreamConn) {
	var msgID string
	if target != "" {
		msgID = uc.appendLog(target, msg)
	}

	// Don't forward messages if it's a detached channel
	if ch, ok := uc.network.channels[target]; ok && ch.Detached {
		return
	}

	uc.forEachDownstream(func(dc *downstreamConn) {
		if dc != origin || dc.caps["echo-message"] {
			dc.sendMessageWithID(dc.marshalMessage(msg, uc.network), msgID)
		} else {
			dc.advanceMessageWithID(msg, msgID)
		}
	})
}

func (uc *upstreamConn) updateAway() {
	away := true
	uc.forEachDownstream(func(*downstreamConn) {
		away = false
	})
	if away == uc.away {
		return
	}
	if away {
		uc.SendMessage(&irc.Message{
			Command: "AWAY",
			Params:  []string{"Auto away"},
		})
	} else {
		uc.SendMessage(&irc.Message{
			Command: "AWAY",
		})
	}
	uc.away = away
}

func (uc *upstreamConn) updateChannelAutoDetach(name string) {
	if uch, ok := uc.channels[name]; ok {
		if ch, ok := uc.network.channels[name]; ok && !ch.Detached {
			uch.updateAutoDetach(ch.DetachAfter)
		}
	}
}
