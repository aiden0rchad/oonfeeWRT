// Command hostilessh is a stand-in for a device that answers SSH and then
// cannot do anything, for driving the un-adopt failure path by hand.
//
// It accepts any password and fails every command with the stderr uci produces
// on a full or read-only overlay. That is the combination `Unadopt` cannot
// express as either success or failure: the connection and the login work, so
// phase 2 runs, and `RemoveFootprint` reports that the login and ACL file are
// still there. The call returns a result AND an error, which is the case that
// used to reach the browser as a bare error string with the residue list
// discarded (STATUS §5aj).
//
// It exists because the alternative way to stage this is a wrong password
// against a real device, and the reference hardware accepts any password when
// root has none — so the "failure" would succeed and un-adopt a working AP.
//
//	go run ./tools/hostilessh &
//	sqlite3 .run/oonfeewrt.db "INSERT INTO devices (mac,host,name,role,adopted_at,caps_json) \
//	  VALUES ('de:ad:be:ef:00:02','127.0.0.1:2222','fake-readonly-overlay','ap',strftime('%s','now'),'{}')"
//
// Then un-adopt it from the Devices screen with any credential. Delete the row
// afterwards; it is not tracked by the collector, so nothing polls it.
package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"fmt"
	"log"
	"net"

	"golang.org/x/crypto/ssh"
)

func main() {
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		log.Fatal(err)
	}
	signer, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		log.Fatal(err)
	}
	cfg := &ssh.ServerConfig{
		PasswordCallback: func(ssh.ConnMetadata, []byte) (*ssh.Permissions, error) {
			return &ssh.Permissions{}, nil
		},
	}
	cfg.AddHostKey(signer)

	ln, err := net.Listen("tcp", "127.0.0.1:2222")
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println("listening on", ln.Addr(), "host key", ssh.FingerprintSHA256(signer.PublicKey()))

	for {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		go serve(conn, cfg)
	}
}

func serve(conn net.Conn, cfg *ssh.ServerConfig) {
	sc, chans, reqs, err := ssh.NewServerConn(conn, cfg)
	if err != nil {
		conn.Close()
		return
	}
	defer sc.Close()
	go ssh.DiscardRequests(reqs)
	for nc := range chans {
		if nc.ChannelType() != "session" {
			_ = nc.Reject(ssh.UnknownChannelType, "session only")
			continue
		}
		ch, chReqs, err := nc.Accept()
		if err != nil {
			continue
		}
		go func() {
			for req := range chReqs {
				if req.WantReply {
					_ = req.Reply(req.Type == "exec", nil)
				}
				if req.Type != "exec" {
					continue
				}
				// Every command fails, the way uci does when it cannot write.
				_, _ = ch.Stderr().Write([]byte(
					"uci: Cannot write to file: Read-only file system\n"))
				_, _ = ch.SendRequest("exit-status", false,
					ssh.Marshal(struct{ Status uint32 }{1}))
				_ = ch.Close()
			}
		}()
	}
}
