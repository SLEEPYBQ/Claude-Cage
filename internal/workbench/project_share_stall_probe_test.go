//go:build !windows

package workbench

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

func TestProjectShareReuseWithStalledGuestRead(t *testing.T) {
	if _, err := os.Stat(sshExecutable()); err != nil {
		t.Skip("OpenSSH unavailable")
	}
	sleep, err := exec.LookPath("sleep")
	if err != nil {
		t.Skip("sleep unavailable")
	}
	s, folder, _ := groupFixture(t)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.NewSignerFromKey(key)
	if err != nil {
		t.Fatal(err)
	}
	config := &ssh.ServerConfig{NoClientAuth: true}
	config.AddHostKey(signer)
	marker := "cage-test-read-probe"
	if err := os.WriteFile(filepath.Join(folder, marker), []byte("readable on host"), 0600); err != nil {
		t.Fatal(err)
	}
	var reads, preparations atomic.Int32
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			func() {
				defer conn.Close()
				_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
				server, channels, requests, err := ssh.NewServerConn(conn, config)
				if err != nil {
					return
				}
				defer server.Close()
				go ssh.DiscardRequests(requests)
				for incoming := range channels {
					channel, requests, err := incoming.Accept()
					if err != nil {
						return
					}
					for request := range requests {
						var payload struct{ Command string }
						if request.Type != "exec" || ssh.Unmarshal(request.Payload, &payload) != nil {
							_ = request.Reply(false, nil)
							continue
						}
						_ = request.Reply(true, nil)
						status := uint32(0)
						switch {
						case strings.Contains(payload.Command, marker):
							// Accept the SSH command but never complete the requested directory read.
							reads.Add(1)
							continue
						case strings.Contains(payload.Command, "mountpoint"):
							preparations.Add(1)
							status = 1
						default:
							_, _ = channel.Write([]byte("guest control responsive"))
						}
						_, _ = channel.SendRequest("exit-status", false, ssh.Marshal(struct{ Status uint32 }{status}))
						_ = channel.Close()
						break
					}
				}
			}()
		}
	}()
	if err := os.MkdirAll(s.instanceDir(), 0700); err != nil {
		t.Fatal(err)
	}
	clientConfig := fmt.Sprintf("Host *\n HostName 127.0.0.1\n Port %d\n User cage\n StrictHostKeyChecking no\n UserKnownHostsFile %s\n", listener.Addr().(*net.TCPAddr).Port, os.DevNull)
	if err := os.WriteFile(filepath.Join(s.instanceDir(), "ssh.config"), []byte(clientConfig), 0600); err != nil {
		t.Fatal(err)
	}
	// Run the genuine host transport supervisor with a deliberately idle child.
	share, err := startProjectTransport(exec.Command(sleep, "30"), folder)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(share.close)
	s.projectShares[folder] = share

	checkCtx, cancelCheck := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancelCheck()
	if out, err := s.guest(checkCtx, "printf 'guest control responsive'"); err != nil || string(out) != "guest control responsive" {
		t.Fatalf("guest control response %q, error %v", out, err)
	}
	readCtx, cancelRead := context.WithTimeout(context.Background(), time.Second)
	defer cancelRead()
	_, readErr := s.guest(readCtx, "cat '"+guestPath(filepath.Join(folder, marker))+"'")
	if readErr == nil || readCtx.Err() != context.DeadlineExceeded || reads.Load() != 1 {
		t.Fatalf("expected one stalled guest read, got %d reads, command error %v, context error %v", reads.Load(), readErr, readCtx.Err())
	}
	t.Log("SSH control responded normally, but the simulated project-file read timed out")

	select {
	case <-share.done:
		t.Fatal("share transport exited before project submission")
	default:
	}
	save, _ := json.Marshal(projectGroup{Name: "test project", Folders: []string{folder}})
	open, _ := json.Marshal(map[string]string{"workspace": folder})
	for _, tc := range []struct {
		path string
		body []byte
	}{
		{"/api/projects/save", save},
		{"/api/projects/open", open},
	} {
		resp := request(s, "POST", tc.path, s.origin, string(tc.body), true)
		if resp.Code != 200 || preparations.Load() != 0 || reads.Load() != 1 {
			t.Fatalf("%s returned HTTP %d (%s), preparation commands %d, reads %d", tc.path, resp.Code, resp.Body.String(), preparations.Load(), reads.Load())
		}
		t.Logf("%s returned HTTP %d with zero guest reads or mount checks during submission", tc.path, resp.Code)
	}

	share.close()
	resp := request(s, "POST", "/api/projects/open", s.origin, string(open), true)
	if resp.Code != 400 || !strings.Contains(resp.Body.String(), "无法在 Cage 中准备项目目录") || preparations.Load() != 1 {
		t.Fatalf("stopped transport probe returned HTTP %d (%s), preparation commands %d", resp.Code, resp.Body.String(), preparations.Load())
	}
	t.Logf("after transport exit, project open attempted mount preparation and returned HTTP %d (%s)", resp.Code, strings.TrimSpace(resp.Body.String()))
}
