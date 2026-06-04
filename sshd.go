package main

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"
	"unsafe"

	"github.com/creack/pty"
	"github.com/gliderlabs/ssh"
	"github.com/gorilla/websocket"
	"github.com/pkg/sftp"
)

func setWinsize(f *os.File, w, h int) {
	syscall.Syscall(syscall.SYS_IOCTL, f.Fd(), uintptr(syscall.TIOCSWINSZ),
		uintptr(unsafe.Pointer(&struct{ h, w, x, y uint16 }{uint16(h), uint16(w), 0, 0})))
}

func getShell() string {
	for _, sh := range []string{"/bin/bash", "/bin/sh"} {
		if _, err := os.Stat(sh); err == nil {
			return sh
		}
	}
	return "/bin/sh"
}

// wsConn wraps gorilla/websocket as net.Conn
type wsConn struct {
	ws     *websocket.Conn
	reader io.Reader
}

func (c *wsConn) Read(b []byte) (int, error) {
	for {
		if c.reader != nil {
			n, err := c.reader.Read(b)
			if n > 0 {
				return n, nil
			}
			c.reader = nil
			if err != nil && err != io.EOF {
				return 0, err
			}
		}
		_, r, err := c.ws.NextReader()
		if err != nil {
			return 0, err
		}
		c.reader = r
	}
}

func (c *wsConn) Write(b []byte) (int, error) {
	err := c.ws.WriteMessage(websocket.BinaryMessage, b)
	if err != nil {
		return 0, err
	}
	return len(b), nil
}

func (c *wsConn) Close() error                       { return c.ws.Close() }
func (c *wsConn) LocalAddr() net.Addr                { return c.ws.LocalAddr() }
func (c *wsConn) RemoteAddr() net.Addr               { return c.ws.RemoteAddr() }
func (c *wsConn) SetDeadline(t time.Time) error      { return nil }
func (c *wsConn) SetReadDeadline(t time.Time) error  { return nil }
func (c *wsConn) SetWriteDeadline(t time.Time) error { return nil }

func main() {
	port := os.Getenv("PORT")
	if port == "" {
		fmt.Fprintf(os.Stderr, "PORT env required\n")
		os.Exit(1)
	}
	password := os.Getenv("PW")
	if password == "" {
		fmt.Fprintf(os.Stderr, "PW env required\n")
		os.Exit(1)
	}
	shell := getShell()

	handler := func(s ssh.Session) {
		cmdArgs := s.Command()
		ptyReq, winCh, isPty := s.Pty()

		if isPty {
			cmd := exec.Command(shell)
			cmd.Env = append(os.Environ(), fmt.Sprintf("TERM=%s", ptyReq.Term))
			f, err := pty.Start(cmd)
			if err != nil {
				io.WriteString(s, fmt.Sprintf("Error: %v\n", err))
				s.Exit(1)
				return
			}
			go func() {
				for win := range winCh {
					setWinsize(f, win.Width, win.Height)
				}
			}()
			go func() {
				io.Copy(f, s)
			}()
			io.Copy(s, f)
			cmd.Wait()
		} else if len(cmdArgs) > 0 {
			cmd := exec.Command(shell, "-c", strings.Join(cmdArgs, " "))
			cmd.Env = os.Environ()
			cmd.Stdout = s
			cmd.Stderr = s
			cmd.Stdin = s
			if err := cmd.Run(); err != nil {
				if exitErr, ok := err.(*exec.ExitError); ok {
					s.Exit(exitErr.ExitCode())
				} else {
					s.Exit(1)
				}
				return
			}
			s.Exit(0)
		} else {
			cmd := exec.Command(shell)
			cmd.Env = os.Environ()
			cmd.Stdout = s
			cmd.Stderr = s
			cmd.Stdin = s
			cmd.Run()
		}
	}

	sftpHandler := func(s ssh.Session) {
		server, err := sftp.NewServer(s)
		if err != nil {
			log.Printf("sftp error: %v\n", err)
			return
		}
		if err := server.Serve(); err == io.EOF {
			server.Close()
		} else if err != nil {
			log.Printf("sftp serve error: %v\n", err)
		}
	}

	sshServer := &ssh.Server{
		Handler: handler,
		PasswordHandler: func(ctx ssh.Context, pass string) bool {
			return pass == password
		},
		SubsystemHandlers: map[string]ssh.SubsystemHandler{
			"sftp": sftpHandler,
		},
	}
	sshServer.SetOption(ssh.HostKeyPEM(generateKey()))

	addr := ":" + port

	log.Printf("sshd listening on %s (shell=%s, sftp=yes, proto=ssh+ws)\n", addr, shell)

	// Multiplexed listener: SSH and WebSocket on same port
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		log.Fatal(err)
	}

	upgrader := websocket.Upgrader{
		CheckOrigin: func(r *http.Request) bool { return true },
	}

	// Channel-based listeners for SSH server
	sshLn := &chanListener{ch: make(chan net.Conn, 16)}

	// SSH server in background
	go sshServer.Serve(sshLn)

	for {
		conn, err := ln.Accept()
		if err != nil {
			continue
		}
		go func(c net.Conn) {
			// Read first byte to detect protocol
			buf := make([]byte, 1)
			c.SetReadDeadline(time.Now().Add(3 * time.Second))
			n, err := c.Read(buf)
			c.SetReadDeadline(time.Time{})
			if err != nil || n == 0 {
				c.Close()
				return
			}

			pConn := &prefixConn{Conn: c, prefix: buf[:n], prefixRead: false}

			if buf[0] == 'S' {
				// SSH (client banner starts with "SSH-2.0-...")
				sshLn.ch <- pConn
			} else {
				// HTTP/WebSocket (starts with "GET" etc)
				wsLn := &chanListener{ch: make(chan net.Conn, 1)}
				wsLn.ch <- pConn
				srv := &http.Server{
					Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						ws, wsErr := upgrader.Upgrade(w, r, nil)
						if wsErr != nil {
							return
						}
						wc := &wsConn{ws: ws}
						sshLn.ch <- wc
					}),
				}
				srv.Serve(wsLn)
			}
		}(conn)
	}
}

func handleConn(c net.Conn, sshSrv *ssh.Server, upgrader *websocket.Upgrader) {
	// SSH: server speaks first (sends banner). Client waits.
	// HTTP/WS: client speaks first (sends "GET / HTTP...").
	// Strategy: set short read deadline. If client sends data -> HTTP. Timeout -> SSH.
	buf := make([]byte, 1)
	c.SetReadDeadline(time.Now().Add(200 * time.Millisecond))
	n, err := c.Read(buf)
	c.SetReadDeadline(time.Time{})

	if n > 0 && err == nil {
		// Client sent data first -> HTTP/WebSocket
		pConn := &prefixConn{Conn: c, prefix: buf[:n], prefixRead: false}
		ln := &chanListener{ch: make(chan net.Conn, 1)}
		ln.ch <- pConn
		srv := &http.Server{
			Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				ws, wsErr := upgrader.Upgrade(w, r, nil)
				if wsErr != nil {
					return
				}
				conn := &wsConn{ws: ws}
				sshSrv.HandleConn(conn)
			}),
		}
		srv.Serve(ln)
	} else {
		// Timeout (no client data) -> SSH (server speaks first)
		sshSrv.HandleConn(c)
	}
}

// prefixConn prepends already-read bytes back to the stream
type prefixConn struct {
	net.Conn
	prefix     []byte
	prefixRead bool
}

func (c *prefixConn) Read(b []byte) (int, error) {
	if !c.prefixRead {
		c.prefixRead = true
		n := copy(b, c.prefix)
		return n, nil
	}
	return c.Conn.Read(b)
}

// chanListener delivers one conn then blocks
type chanListener struct {
	ch   chan net.Conn
	addr net.Addr
}

func (l *chanListener) Accept() (net.Conn, error) {
	c, ok := <-l.ch
	if !ok {
		return nil, fmt.Errorf("closed")
	}
	l.addr = c.LocalAddr()
	return c, nil
}
func (l *chanListener) Close() error   { return nil }
func (l *chanListener) Addr() net.Addr {
	if l.addr != nil {
		return l.addr
	}
	return &net.TCPAddr{IP: net.IPv4zero, Port: 0}
}

func generateKey() []byte {
	key, _ := rsa.GenerateKey(rand.Reader, 2048)
	der := x509.MarshalPKCS1PrivateKey(key)
	block := &pem.Block{Type: "RSA PRIVATE KEY", Bytes: der}
	return pem.EncodeToMemory(block)
}
