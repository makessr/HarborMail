package server

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"net"
	"net/textproto"
	"strconv"
	"strings"
	"sync"
	"time"
)

func (a *app) listenPOP3S(runtime *serviceRuntime, addr string, config *tls.Config) error {
	if addr == "" {
		return nil
	}
	if config == nil {
		return errors.New("POP3S requires TLS certificate configuration")
	}
	listener, err := tls.Listen("tcp", addr, config.Clone())
	if err != nil {
		return fmt.Errorf("listen for POP3S: %w", err)
	}
	log.Printf("POP3S listening on %s", listener.Addr())
	runtime.serve("POP3S", func() error {
		server := popServer{app: a, slots: make(chan struct{}, 64)}
		ctx, cancel := context.WithCancel(runtime.ctx)
		var connections sync.WaitGroup
		defer func() {
			cancel()
			connections.Wait()
		}()
		for {
			conn, err := listener.Accept()
			if err != nil {
				return err
			}
			select {
			case server.slots <- struct{}{}:
				connections.Go(func() {
					stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
					defer stop()
					server.serve(conn.(*tls.Conn))
				})
			default:
				_ = conn.Close()
			}
		}
	})
	runtime.stops = append(runtime.stops, func(context.Context) { _ = listener.Close() })
	return nil
}

type popServer struct {
	app      *app
	slots    chan struct{}
	maildrop sync.Map
}

type popMessage struct {
	id      int64
	size    int64
	deleted bool
}

type popSession struct {
	server   *popServer
	writer   *textproto.Writer
	username string
	owner    string
	messages []popMessage
	failures int
	peer     net.Addr
}

func (server *popServer) serve(conn *tls.Conn) {
	defer func() { <-server.slots }()
	defer conn.Close()
	if err := conn.SetDeadline(time.Now().Add(10 * time.Second)); err != nil {
		return
	}
	if err := conn.Handshake(); err != nil {
		return
	}
	reader := bufio.NewReaderSize(conn, 2048)
	s := popSession{server: server, writer: textproto.NewWriter(bufio.NewWriter(conn)), peer: conn.RemoteAddr()}
	defer func() {
		if s.owner != "" {
			server.maildrop.Delete(s.owner)
		}
	}()
	if err := s.writer.PrintfLine("+OK Harbor Mail ready"); err != nil {
		return
	}
	for {
		if err := conn.SetReadDeadline(time.Now().Add(10 * time.Minute)); err != nil {
			return
		}
		line, err := reader.ReadSlice('\n')
		if err != nil || !bytes.HasSuffix(line, []byte("\r\n")) {
			return
		}
		if err := conn.SetWriteDeadline(time.Now().Add(30 * time.Second)); err != nil {
			return
		}
		verb, arg, _ := strings.Cut(string(line[:len(line)-2]), " ")
		closeConnection, err := s.command(strings.ToUpper(verb), arg)
		if closeConnection || err != nil {
			return
		}
	}
}

func (s *popSession) command(verb, arg string) (bool, error) {
	args := strings.Fields(arg)
	switch verb {
	case "CAPA":
		if len(args) == 0 {
			return false, s.multiline("Capability list", []byte("USER\r\nUIDL\r\nTOP\r\n"))
		}
	case "QUIT":
		if len(args) == 0 {
			if err := s.commitDeletions(); err != nil {
				log.Print("POP3 deletion commit: ", err)
				return true, s.writer.PrintfLine("-ERR Could not update maildrop")
			}
			return true, s.writer.PrintfLine("+OK Goodbye")
		}
	}
	if s.owner == "" {
		switch verb {
		case "USER":
			if len(args) == 1 {
				s.username = args[0]
				return false, s.writer.PrintfLine("+OK Username accepted")
			}
		case "PASS":
			if s.username != "" && arg != "" {
				return s.login(arg)
			}
		}
		return false, s.writer.PrintfLine("-ERR Authenticate with USER and PASS")
	}
	return false, s.transaction(verb, args)
}

func (s *popSession) login(password string) (bool, error) {
	owner, err := s.server.app.authenticateProtocol(s.peer, s.username, password, clientPOP3)
	s.username = ""
	if errors.Is(err, errLoginBusy) {
		return false, s.writer.PrintfLine("-ERR Authentication temporarily unavailable")
	}
	if err != nil {
		s.failures++
		return s.failures >= 3, s.writer.PrintfLine("-ERR Invalid credentials")
	}
	if _, busy := s.server.maildrop.LoadOrStore(owner, struct{}{}); busy {
		return false, s.writer.PrintfLine("-ERR Maildrop already in use")
	}
	messages, err := s.server.app.popSnapshot(owner)
	if err != nil {
		s.server.maildrop.Delete(owner)
		log.Print("POP3 maildrop snapshot: ", err)
		return false, s.writer.PrintfLine("-ERR Could not open maildrop")
	}
	s.messages = messages
	s.owner = owner
	return false, s.writer.PrintfLine("+OK Maildrop ready")
}

func (a *app) popSnapshot(owner string) ([]popMessage, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	rows, err := a.db.QueryContext(ctx, `SELECT id,pop_octets,CASE WHEN pop_octets IS NULL THEN raw END
		FROM letters WHERE owner=? AND folder='inbox' ORDER BY id`, owner)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var messages []popMessage
	for rows.Next() {
		var m popMessage
		var size sql.NullInt64
		var legacyRaw sql.RawBytes
		if err := rows.Scan(&m.id, &size, &legacyRaw); err != nil {
			return nil, err
		}
		m.size = size.Int64
		if !size.Valid {
			m.size = popOctets(legacyRaw)
		}
		messages = append(messages, m)
	}
	return messages, rows.Err()
}

func (a *app) backfillPOPOctets(ctx context.Context) error {
	var after int64
	for {
		// Read one message at a time and release the connection before updating it.
		var id int64
		var raw []byte
		err := a.db.QueryRowContext(ctx, `SELECT id,raw FROM letters
			WHERE id>? AND pop_octets IS NULL ORDER BY id LIMIT 1`, after).Scan(&id, &raw)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		if _, err := a.db.ExecContext(ctx, `UPDATE letters SET pop_octets=?
			WHERE id=? AND pop_octets IS NULL AND raw=COALESCE(?,X'')`, popOctets(raw), id, raw); err != nil {
			return err
		}
		after = id
	}
}

// POP3 sizes include CRLF line endings but exclude dot escaping and the terminator.
func popOctets(raw []byte) int64 {
	size := int64(len(raw) + bytes.Count(raw, []byte("\n")) - bytes.Count(raw, []byte("\r\n")))
	if len(raw) != 0 {
		switch raw[len(raw)-1] {
		case '\n':
		case '\r':
			size++
		default:
			size += 2
		}
	}
	return size
}

func (s *popSession) transaction(verb string, args []string) error {
	if len(args) == 0 {
		switch verb {
		case "NOOP":
			return s.writer.PrintfLine("+OK")
		case "RSET":
			for i := range s.messages {
				s.messages[i].deleted = false
			}
			return s.writer.PrintfLine("+OK Deletion marks cleared")
		case "STAT":
			var count int
			var size int64
			for _, m := range s.messages {
				if !m.deleted {
					count++
					size += m.size
				}
			}
			return s.writer.PrintfLine("+OK %d %d", count, size)
		case "LIST", "UIDL":
			var listing bytes.Buffer
			for i, m := range s.messages {
				if !m.deleted {
					fmt.Fprintf(&listing, "%d %s\r\n", i+1, m.listValue(verb))
				}
			}
			return s.multiline("Maildrop listing", listing.Bytes())
		}
	}

	if (len(args) == 1 && (verb == "LIST" || verb == "UIDL" || verb == "DELE" || verb == "RETR")) ||
		(len(args) == 2 && verb == "TOP") {
		number, err := strconv.Atoi(args[0])
		if err != nil || number < 1 || number > len(s.messages) || s.messages[number-1].deleted {
			return s.writer.PrintfLine("-ERR No such message")
		}
		m := &s.messages[number-1]
		switch verb {
		case "LIST", "UIDL":
			return s.writer.PrintfLine("+OK %d %s", number, m.listValue(verb))
		case "DELE":
			m.deleted = true
			return s.writer.PrintfLine("+OK Message marked")
		case "RETR":
			return s.retrieve(m, -1)
		case "TOP":
			lines, err := strconv.Atoi(args[1])
			if err == nil && lines >= 0 {
				return s.retrieve(m, lines)
			}
		}
	}
	return s.writer.PrintfLine("-ERR Unknown command or invalid argument")
}

func (m popMessage) listValue(verb string) string {
	if verb == "UIDL" {
		return "harbor-" + strconv.FormatInt(m.id, 10)
	}
	return strconv.FormatInt(m.size, 10)
}

func (s *popSession) retrieve(m *popMessage, lines int) error {
	raw, err := s.server.app.rawLetter(s.owner, m.id)
	if err != nil {
		return s.writer.PrintfLine("-ERR Could not read message")
	}
	if lines >= 0 {
		return s.multiline("Message preview", popTop(raw, lines))
	}
	return s.multiline(fmt.Sprintf("%d octets", m.size), raw)
}

func popTop(raw []byte, lines int) []byte {
	for offset := 0; offset < len(raw); {
		end := bytes.IndexByte(raw[offset:], '\n')
		if end < 0 {
			break
		}
		part := raw[offset : offset+end]
		offset += end + 1
		if len(part) == 0 || bytes.Equal(part, []byte("\r")) {
			for lines > 0 && offset < len(raw) {
				end = bytes.IndexByte(raw[offset:], '\n')
				if end < 0 {
					return raw
				}
				offset += end + 1
				lines--
			}
			return raw[:offset]
		}
	}
	return raw
}

func (s *popSession) multiline(status string, data []byte) error {
	if err := s.writer.PrintfLine("+OK %s", status); err != nil {
		return err
	}
	writer := s.writer.W
	for len(data) != 0 {
		line, rest, _ := bytes.Cut(data, []byte("\n"))
		data = rest
		if len(line) > 0 && line[0] == '.' {
			if err := writer.WriteByte('.'); err != nil {
				return err
			}
		}
		if _, err := writer.Write(line); err != nil {
			return err
		}
		ending := "\r\n"
		if len(line) > 0 && line[len(line)-1] == '\r' {
			ending = "\n"
		}
		if _, err := writer.WriteString(ending); err != nil {
			return err
		}
	}
	return s.writer.PrintfLine(".")
}

func (s *popSession) commitDeletions() error {
	for _, m := range s.messages {
		if m.deleted {
			return s.moveMarkedToTrash()
		}
	}
	return nil
}

func (s *popSession) moveMarkedToTrash() error {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	tx, err := s.server.app.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := lockIMAPUIDs(ctx, tx); err != nil {
		return err
	}
	for _, m := range s.messages {
		if !m.deleted {
			continue
		}
		if _, err := tx.ExecContext(ctx, `UPDATE letters SET previous='inbox', folder='trash'
			WHERE owner=? AND id=? AND folder='inbox'`, s.owner, m.id); err != nil {
			return err
		}
		if err := invalidateMovedUID(ctx, tx, m.id); err != nil {
			return err
		}
	}
	return s.server.app.commitMailbox(tx, s.owner)
}
