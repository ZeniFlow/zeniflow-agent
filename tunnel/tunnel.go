// Package tunnel — собственный обратный туннель поверх mTLS.
//
// Сервер стоит на публичном адресе, клиент (агент ноды) сам подключается к нему: входящие порты на ноде не нужны.
// Публичное TCP-соединение пересылается на адрес внутри сети ноды. Мультиплексирования нет: на каждое соединение
// клиент открывает отдельное TLS-соединение к серверу.
//
// Протокол, по строке JSON на сообщение:
//   - клиент открывает «управляющее» соединение: {"op":"control"}; сервер шлёт в него {"id","target"} на каждое новое
//     публичное соединение и {"ping":true} раз в pingEvery;
//   - клиент в ответ открывает «данные»: {"op":"data","id":...}; сервер склеивает его с ожидающим публичным соединением.
//
// Кто клиент, определяется только по проверенному сертификату (CN), а не по словам клиента.
package tunnel

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"sync"
	"time"
)

const (
	pingEvery      = 20 * time.Second
	clientTimeout  = 3 * pingEvery // клиент не слышит сервер дольше этого срока: соединение считается мёртвым
	openTimeout    = 10 * time.Second
	maxPending     = 256 // ожидающих открытий на одного клиента: защита от переполнения
	handshakeLimit = 10 * time.Second
	maxLine        = 4 << 10
)

type hello struct {
	Op string `json:"op"`
	ID string `json:"id,omitempty"`
}

type openMsg struct {
	ID     string `json:"id,omitempty"`
	Target string `json:"target,omitempty"`
	Ping   bool   `json:"ping,omitempty"`
}

var ErrClientOffline = errors.New("туннель клиента не подключён")

// readLine читает одну короткую строку JSON, не позволяя собеседнику прислать бесконечную строку.
func readLine(r *bufio.Reader, v any) error {
	line, err := r.ReadSlice('\n')
	if errors.Is(err, bufio.ErrBufferFull) {
		return errors.New("слишком длинное сообщение")
	}
	if err != nil {
		return err
	}
	return json.Unmarshal(line, v)
}

func writeLine(w io.Writer, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	_, err = w.Write(append(b, '\n'))
	return err
}

// pipe копирует данные в обе стороны и закрывает оба соединения, когда закончилась любая сторона.
func pipe(a, b net.Conn) {
	var once sync.Once
	closeBoth := func() { _ = a.Close(); _ = b.Close() }
	done := make(chan struct{}, 2)
	cp := func(dst, src net.Conn) {
		_, _ = io.Copy(dst, src)
		// Половинное закрытие: собеседник дочитает остаток, но запись в эту сторону завершена.
		if cw, ok := dst.(interface{ CloseWrite() error }); ok {
			_ = cw.CloseWrite()
		}
		done <- struct{}{}
	}
	go cp(a, b)
	go cp(b, a)
	<-done
	<-done
	once.Do(closeBoth)
}

// --- Сервер ---

type pending struct {
	client string
	ch     chan net.Conn
}

type control struct {
	conn net.Conn
	mu   sync.Mutex // запись в управляющее соединение из разных горутин
	enc  *json.Encoder
}

// Server принимает туннели клиентов и пересылает публичные соединения в них.
type Server struct {
	mu       sync.Mutex
	controls map[string]*control // по CN сертификата клиента
	pending  map[string]*pending
}

func NewServer() *Server {
	return &Server{controls: map[string]*control{}, pending: map[string]*pending{}}
}

// Serve принимает соединения клиентов на ln (должен быть TLS-слушатель с обязательной проверкой клиентского сертификата).
func (s *Server) Serve(ctx context.Context, ln net.Listener) error {
	go func() { <-ctx.Done(); _ = ln.Close() }()
	for {
		c, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		go s.handle(ctx, c)
	}
}

func clientID(c net.Conn) (string, error) {
	tc, ok := c.(*tls.Conn)
	if !ok {
		return "", errors.New("нужно TLS-соединение")
	}
	if err := tc.Handshake(); err != nil {
		return "", err
	}
	st := tc.ConnectionState()
	if len(st.VerifiedChains) == 0 || len(st.VerifiedChains[0]) == 0 {
		return "", errors.New("нет проверенного сертификата клиента")
	}
	return st.VerifiedChains[0][0].Subject.CommonName, nil
}

func (s *Server) handle(ctx context.Context, c net.Conn) {
	_ = c.SetDeadline(time.Now().Add(handshakeLimit))
	id, err := clientID(c)
	if err != nil {
		_ = c.Close()
		return
	}
	r := bufio.NewReaderSize(c, maxLine)
	var h hello
	if err := readLine(r, &h); err != nil {
		_ = c.Close()
		return
	}
	switch h.Op {
	case "control":
		_ = c.SetDeadline(time.Time{})
		s.runControl(ctx, id, c)
	case "data":
		s.mu.Lock()
		p := s.pending[h.ID]
		if p != nil && p.client == id { // отвечать на чужое открытие нельзя
			delete(s.pending, h.ID)
		} else {
			p = nil
		}
		s.mu.Unlock()
		if p == nil {
			_ = c.Close()
			return
		}
		_ = c.SetDeadline(time.Time{})
		// Буфер bufio мог уже забрать начало данных клиента: возвращаем его вместе с соединением.
		p.ch <- &bufConn{Conn: c, r: r}
	default:
		_ = c.Close()
	}
}

// bufConn отдаёт сначала то, что уже прочитано в буфер, а потом остальное из соединения.
type bufConn struct {
	net.Conn
	r *bufio.Reader
}

func (b *bufConn) Read(p []byte) (int, error) { return b.r.Read(p) }
func (b *bufConn) CloseWrite() error {
	if cw, ok := b.Conn.(interface{ CloseWrite() error }); ok {
		return cw.CloseWrite()
	}
	return nil
}

func (s *Server) runControl(ctx context.Context, id string, c net.Conn) {
	ctl := &control{conn: c, enc: json.NewEncoder(c)}
	s.mu.Lock()
	if old := s.controls[id]; old != nil {
		_ = old.conn.Close() // новое подключение вытесняет старое
	}
	s.controls[id] = ctl
	s.mu.Unlock()
	slog.Info("туннель подключён", "client", id)
	defer func() {
		s.mu.Lock()
		if s.controls[id] == ctl {
			delete(s.controls, id)
		}
		s.mu.Unlock()
		_ = c.Close()
		slog.Info("туннель отключён", "client", id)
	}()

	t := time.NewTicker(pingEvery)
	defer t.Stop()
	closed := make(chan struct{})
	go func() { _, _ = io.Copy(io.Discard, c); close(closed) }() // клиент ничего не шлёт; чтение нужно, чтобы заметить обрыв
	for {
		select {
		case <-ctx.Done():
			return
		case <-closed:
			return
		case <-t.C:
			if err := ctl.send(openMsg{Ping: true}); err != nil {
				return
			}
		}
	}
}

func (c *control) send(m openMsg) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	_ = c.conn.SetWriteDeadline(time.Now().Add(handshakeLimit))
	return c.enc.Encode(m)
}

// Connect просит клиента open открыть соединение к target внутри его сети и возвращает готовое соединение.
func (s *Server) Connect(ctx context.Context, client, target string) (net.Conn, error) {
	var idb [12]byte
	_, _ = rand.Read(idb[:])
	id := hex.EncodeToString(idb[:])
	p := &pending{client: client, ch: make(chan net.Conn, 1)}

	s.mu.Lock()
	ctl := s.controls[client]
	n := 0
	for _, q := range s.pending {
		if q.client == client {
			n++
		}
	}
	if ctl == nil {
		s.mu.Unlock()
		return nil, ErrClientOffline
	}
	if n >= maxPending {
		s.mu.Unlock()
		return nil, errors.New("слишком много ожидающих открытий")
	}
	s.pending[id] = p
	s.mu.Unlock()
	drop := func() { s.mu.Lock(); delete(s.pending, id); s.mu.Unlock() }

	if err := ctl.send(openMsg{ID: id, Target: target}); err != nil {
		drop()
		return nil, err
	}
	select {
	case c := <-p.ch:
		return c, nil
	case <-time.After(openTimeout):
		drop()
		return nil, errors.New("клиент не ответил вовремя")
	case <-ctx.Done():
		drop()
		return nil, ctx.Err()
	}
}

// Forward принимает публичные соединения на ln и пересылает каждое клиенту client на target. Возвращается, когда ln закрыт.
func (s *Server) Forward(ctx context.Context, ln net.Listener, client, target string) {
	go func() { <-ctx.Done(); _ = ln.Close() }()
	for {
		pub, err := ln.Accept()
		if err != nil {
			return
		}
		go func() {
			cctx, cancel := context.WithTimeout(ctx, openTimeout+time.Second)
			defer cancel()
			tun, err := s.Connect(cctx, client, target)
			if err != nil {
				slog.Warn("пересылка не удалась", "client", client, "target", target, "err", err)
				_ = pub.Close()
				return
			}
			pipe(pub, tun)
		}()
	}
}

// --- Клиент ---

// Client держит управляющее соединение с сервером и по его просьбам открывает соединения к целям внутри сети ноды.
type Client struct {
	Server string      // host:port сервера туннеля
	TLS    *tls.Config // клиентский сертификат и проверка сервера по CA
	// Allow решает, можно ли открывать соединение к target. Обязателен: иначе сервер мог бы направить клиента в любую точку сети ноды.
	Allow func(target string) bool
	// Dial открывает соединение к цели; по умолчанию net.Dialer.
	Dial func(ctx context.Context, target string) (net.Conn, error)
}

func (c *Client) dialServer(ctx context.Context) (net.Conn, error) {
	d := tls.Dialer{NetDialer: &net.Dialer{Timeout: handshakeLimit, KeepAlive: 15 * time.Second}, Config: c.TLS}
	return d.DialContext(ctx, "tcp", c.Server)
}

// Run работает, пока жив ctx: при обрыве переподключается с нарастающей паузой от секунды до минуты.
func (c *Client) Run(ctx context.Context) {
	backoff := time.Second
	for ctx.Err() == nil {
		started := time.Now()
		err := c.session(ctx)
		if ctx.Err() != nil {
			return
		}
		if time.Since(started) > 30*time.Second {
			backoff = time.Second
		}
		slog.Warn("туннель разорван", "server", c.Server, "err", err, "повтор_через", backoff.String())
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, time.Minute)
	}
}

func (c *Client) session(ctx context.Context) error {
	conn, err := c.dialServer(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	go func() { <-ctx.Done(); _ = conn.Close() }()
	if err := writeLine(conn, hello{Op: "control"}); err != nil {
		return err
	}
	r := bufio.NewReaderSize(conn, maxLine)
	for {
		_ = conn.SetReadDeadline(time.Now().Add(clientTimeout)) // сервер пингует: тишина значит обрыв
		var m openMsg
		if err := readLine(r, &m); err != nil {
			return err
		}
		if m.Ping || m.ID == "" {
			continue
		}
		go c.open(ctx, m)
	}
}

func (c *Client) open(ctx context.Context, m openMsg) {
	if c.Allow == nil || !c.Allow(m.Target) {
		slog.Warn("цель не разрешена", "target", m.Target)
		return
	}
	dial := c.Dial
	if dial == nil {
		dial = func(ctx context.Context, t string) (net.Conn, error) {
			return (&net.Dialer{Timeout: handshakeLimit}).DialContext(ctx, "tcp", t)
		}
	}
	target, err := dial(ctx, m.Target)
	if err != nil {
		slog.Warn("цель недоступна", "target", m.Target, "err", err)
		return
	}
	data, err := c.dialServer(ctx)
	if err != nil {
		_ = target.Close()
		return
	}
	if err := writeLine(data, hello{Op: "data", ID: m.ID}); err != nil {
		_ = target.Close()
		_ = data.Close()
		return
	}
	pipe(target, data)
}

// String нужен для читаемых логов.
func (c *Client) String() string { return fmt.Sprintf("tunnel→%s", c.Server) }
