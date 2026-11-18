//go:build !windows

package workbench

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
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
	if err := s.rememberSharedProject(folder); err != nil {
		t.Fatal(err)
	}
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
	var reads, handoffs atomic.Int32
	var recovered atomic.Bool
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			func() {
				defer conn.Close()
				_ = conn.SetDeadline(time.Now().Add(12 * time.Second))
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
						case strings.Contains(payload.Command, "mountpoint -q --") && strings.Contains(payload.Command, "os.scandir"):
							reads.Add(1)
							if !recovered.Load() {
								// Keep SSH established while the guest directory read stalls.
								continue
							}
						case strings.Contains(payload.Command, "--unix-socket /run/cage-desktop/files.sock"):
							handoffs.Add(1)
							body, err := io.ReadAll(channel)
							var input map[string]string
							if err != nil || json.Unmarshal(body, &input) != nil || input["path"] != guestPath(folder) || input["id"] != testBrowserFileID {
								status = 1
							} else {
								_, _ = channel.Write([]byte("{\"ok\":true}\nCAGE_FILE_HTTP:200"))
							}
						default:
							status = 1
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

	body, err := json.Marshal(map[string]string{"target": "desktop", "id": testBrowserFileID, "workspace": folder})
	if err != nil {
		t.Fatal(err)
	}
	const endpoint = "/api/input/files/directory"
	started := time.Now()
	resp := request(s, "POST", endpoint, s.origin, string(body), true)
	if elapsed := time.Since(started); elapsed >= 8*time.Second {
		t.Fatalf("handoff returned after %s, expected the short probe deadline", elapsed)
	}
	if resp.Code != http.StatusBadRequest || !strings.Contains(resp.Body.String(), "项目文件夹暂时无法读取") || reads.Load() != 1 || handoffs.Load() != 0 {
		t.Fatalf("stalled read: HTTP %d (%s), reads %d, handoffs %d", resp.Code, resp.Body.String(), reads.Load(), handoffs.Load())
	}
	select {
	case <-share.done:
		t.Fatal("failed check closed the in-use transport")
	default:
	}
	if s.projectShares[folder] != share {
		t.Fatal("failed check replaced the existing share")
	}
	t.Log("stalled directory check returned an error before handoff and preserved the existing transport")

	recovered.Store(true)
	resp = request(s, "POST", endpoint, s.origin, string(body), true)
	if resp.Code != http.StatusOK || strings.TrimSpace(resp.Body.String()) != `{"ok":true}` || reads.Load() != 2 || handoffs.Load() != 1 {
		t.Fatalf("recovered read: HTTP %d (%s), reads %d, handoffs %d", resp.Code, resp.Body.String(), reads.Load(), handoffs.Load())
	}
	t.Log("retry after the directory recovered delivered it through the guest file service")
}
