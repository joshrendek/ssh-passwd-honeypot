package main

import (
	"fmt"
	"github.com/satori/go.uuid"
	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/terminal"
	"io"
	"io/ioutil"
	"log"
	"net"
	"os"
	"os/exec"
	"strconv"
	"strings"
)

const DEFAULT_SHELL = "bash"

func Exists(name string) bool {
	if _, err := os.Stat(name); err != nil {
		if os.IsNotExist(err) {
			return false
		}
	}
	return true
}

func generateSshKey() {
	log.Println("[generating ssh keys]")
	if Exists("honeypot") {
		log.Println("[removing old keys]")
		os.Remove("honeypot")
		os.Remove("honeypot.pub")
	}

	out, err := exec.Command("ssh-keygen", "-t", "rsa", "-q", "-f", "honeypot", "-N", "").CombinedOutput()
	if err != nil {
		log.Println(string(out))
		panic(fmt.Sprintf("Error generating key: %s", err))
	}
}

func handleRequests(reqs <-chan *ssh.Request) {
	for req := range reqs {
		log.Printf("recieved out-of-band request: %+v", req)
	}
}

func handleChannels(chans <-chan ssh.NewChannel, perms *ssh.Permissions) {
	// Service the incoming Channel channel.
	for newChannel := range chans {
		// Channels have a type, depending on the application level
		// protocol intended. In the case of a shell, the type is
		// "session" and ServerShell may be used to present a simple
		// terminal interface.
		//if t := newChannel.ChannelType(); t != "session" {
		//	newChannel.Reject(ssh.UnknownChannelType, fmt.Sprintf("unknown channel type: %s", t))
		//	continue
		//}
		channel, requests, err := newChannel.Accept()
		if err != nil {
			log.Printf("could not accept channel (%s)", err)
			continue
		}

		var shell string
		shell = os.Getenv("SHELL")
		if shell == "" {
			shell = DEFAULT_SHELL
		}

		// Sessions have out-of-band requests such as "shell", "pty-req" and "env"
		go func(in <-chan *ssh.Request) {
			for req := range in {
				term := terminal.NewTerminal(channel, "")
				handler := NewCommandHandler(term)
				handler.Register(&Ls{}, &LsAl{},
					&Help{},
					&Pwd{},
					&UnsetHistory{},
					&Uname{},
					&Echo{},
					&Whoami{User: "root"},
				)

				log.Printf("Payload: %s", req.Payload)
				ok := false
				switch req.Type {
				// exec is used: ssh user@host 'some command'
				case "exec":
					ok = true
					command := string(req.Payload[4 : req.Payload[3]+4])

					cmdOut, newLine := handler.MatchAndRun(command)
					term.Write([]byte(cmdOut))
					if newLine {
						term.Write([]byte("\r\n"))
					}

					shellCommand := &ShellCommand{Cmd: command, Guid: perms.Extensions["guid"]}
					go shellCommand.Save()

					channel.Close()
				// shell is used: ssh user@host ... then commands are entered
				case "shell":
					for {
						term.Write([]byte("root@localhost:/# "))
						line, err := term.ReadLine()
						if err == io.EOF {
							log.Printf("EOF detected, closing")
							channel.Close()
							ok = true
							break
						}
						if err != nil {
							log.Printf("Error: %s", err)
						}

						cmdOut, newLine := handler.MatchAndRun(line)
						term.Write([]byte(cmdOut))
						if newLine {
							term.Write([]byte("\r\n"))
						}

						shellCommand := &ShellCommand{Cmd: line, Guid: perms.Extensions["guid"]}
						go shellCommand.Save()

						log.Println(line)
					}
					//cmd := exec.Command(shell)
					//cmd.Env = []string{"TERM=xterm"}

					// We don't accept any commands (Payload),
					// only the default shell.
					if len(req.Payload) == 0 {
						ok = true
					}
				case "pty-req":
					// Responding 'ok' here will let the client
					// know we have a pty ready for input
					ok = true
					// Parse body...
					termLen := req.Payload[3]
					termEnv := string(req.Payload[4 : termLen+4])
					log.Printf("pty-req '%s'", termEnv)
				default:
					log.Printf("[%s] Payload: %s", req.Type, req.Payload)
				}

				if !ok {
					log.Printf("declining %s request...", req.Type)
				}

				req.Reply(ok, nil)
			}
		}(requests)
	}
}

func parseIpPortFrom(conn ssh.ConnMetadata) (string, int) {
	remote := strings.Split(conn.RemoteAddr().String(), ":")
	port, err := strconv.Atoi(remote[1])
	if err != nil {
		port = 0
	}
	return remote[0], port
}

func passAuthCallback(conn ssh.ConnMetadata, password []byte) (*ssh.Permissions, error) {
	guid := uuid.NewV4()
	ip, remotePort := parseIpPortFrom(conn)
	login := SshLogin{RemoteAddr: ip,
		RemotePort: remotePort,
		Username:   conn.User(),
		Password:   string(password),
		Guid:       guid.String(),
		Version:    string(conn.ClientVersion()),
		LoginType:  "password",
	}
	login.Save()
	return &ssh.Permissions{Extensions: map[string]string{"guid": guid.String()}}, nil
}

func keyAuthCallback(conn ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
	guid := uuid.NewV4()
	ip, remotePort := parseIpPortFrom(conn)
	login := SshLogin{RemoteAddr: ip,
		RemotePort: remotePort,
		Username:   conn.User(),
		Guid:       guid.String(),
		Version:    string(conn.ClientVersion()),
		PublicKey:  key.Marshal(),
		KeyType:    string(key.Type()),
		LoginType:  "key",
	}
	go login.Save()
	//log.Println("Fail to authenticate", conn, ":", err)
	//return nil, errors.New("invalid authentication")
	return &ssh.Permissions{Extensions: map[string]string{"guid": guid.String()}}, nil
}

func main() {
	generateSshKey()
	sshConfig := &ssh.ServerConfig{
		PasswordCallback:  passAuthCallback,
		PublicKeyCallback: keyAuthCallback,
		ServerVersion:     "SSH-2.0-OpenSSH_6.4p1, OpenSSL 1.0.1e-fips 11 Feb 2013", // old and vulnerable!
	}

	// You can generate a keypair with 'ssh-keygen -t rsa -C "test@example.com"'
	privateBytes, err := ioutil.ReadFile("./honeypot")
	if err != nil {
		log.Fatal("Failed to load private key (./honeypot)")
	}

	private, err := ssh.ParsePrivateKey(privateBytes)
	if err != nil {
		log.Fatal("Failed to parse private key")
	}

	sshConfig.AddHostKey(private)

	// Accept all connections
	port := os.Getenv("PORT")
	if port == "" {
		port = "22"
	}

	// Once a ServerConfig has been configured, connections can be accepted.
	listener, err := net.Listen("tcp4", ":"+port)
	if err != nil {
		log.Fatalf("failed to listen on *:2022")
	}

	log.Printf("listening on %s", "0.0.0.0:"+port)
	for {
		tcpConn, err := listener.Accept()
		if err != nil {
			log.Printf("failed to accept incoming connection (%s)", err)
			continue
		}
		// Before use, a handshake must be performed on the incoming net.Conn.
		sshConn, chans, reqs, err := ssh.NewServerConn(tcpConn, sshConfig)
		if err != nil {
			log.Printf("failed to handshake (%s)", err)
			continue
		}

		// Check remote address
		log.Printf("new ssh connection from %s (%s)", sshConn.RemoteAddr(), sshConn.ClientVersion())

		// Print incoming out-of-band Requests
		go handleRequests(reqs)
		// Accept all channels
		go handleChannels(chans, sshConn.Permissions)
	}
}
