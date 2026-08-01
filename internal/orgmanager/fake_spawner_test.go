package orgmanager

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"sync"
	"sync/atomic"
	"time"
)

// fakeSpawner stands in for a real tenant process: it serves a real HTTP server
// on a real unix socket and returns a channel-driven Process. That keeps the
// parts under test honest — the proxy, the socket, the readiness pipe, and the
// Wait/signal semantics are all genuine — while costing microseconds instead of
// a process spawn per case.
type fakeSpawner struct {
	// handler serves tenant requests. Defaults to a 200 "ok" responder.
	handler http.Handler

	// failReady makes the child report ok:false with this message.
	failReady string
	// dieBeforeReady makes the child exit without writing anything, which the
	// host must detect as EOF rather than waiting out the spawn timeout.
	dieBeforeReady bool
	// hangForever makes the child never report readiness, forcing the timeout.
	hangForever bool
	// forgeReady makes the child report ok:true WITHOUT binding its socket —
	// the M2 forgery: readiness as a bare self-report.
	forgeReady bool
	// ignoreSIGTERM makes the child refuse to stop politely, forcing the kill.
	ignoreSIGTERM bool

	// unconfined makes Spawner.Confines report false — degraded mode, where the
	// manager must refuse to bind control sockets (L2). The zero value is
	// "confined" so existing tests that exercise the deploy channel keep binding
	// control sockets without change.
	unconfined bool

	// gate, when non-nil, blocks inside Spawn until it is closed. It opens the
	// window between "load has started" and "load publishes" so a test can
	// land an Evict inside it. spawnEntered is closed once Spawn is reached.
	gate         chan struct{}
	spawnEntered chan struct{}
	enteredOnce  sync.Once

	mu      sync.Mutex
	spawned int
	procs   []*fakeProcess
	reqs    []SpawnRequest
}

func (f *fakeSpawner) lastReq() SpawnRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.reqs) == 0 {
		return SpawnRequest{}
	}
	return f.reqs[len(f.reqs)-1]
}

func (f *fakeSpawner) spawnCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.spawned
}

func (f *fakeSpawner) lastProc() *fakeProcess {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.procs) == 0 {
		return nil
	}
	return f.procs[len(f.procs)-1]
}

func (f *fakeSpawner) Confines() bool { return !f.unconfined }

func (f *fakeSpawner) Spawn(ctx context.Context, req SpawnRequest, log *slog.Logger) (Process, error) {
	f.mu.Lock()
	f.spawned++
	f.reqs = append(f.reqs, req)
	f.mu.Unlock()

	proc := &fakeProcess{
		exited:        make(chan struct{}),
		ignoreSIGTERM: f.ignoreSIGTERM,
		pid:           1000 + f.spawnCount(),
	}

	if f.dieBeforeReady {
		// Close the readiness pipe without writing: the host sees EOF.
		req.ReadyFile.Close()
		// Go through die() so the exitOnce guards this like any other exit —
		// the host may still call Kill() on the failure path.
		proc.die(fmt.Errorf("exit status 1"))
		f.record(proc)
		return proc, nil
	}

	if f.forgeReady {
		writeReady(req.ReadyFile, readyMsg{OK: true, PID: proc.pid})
		f.record(proc)
		return proc, nil
	}

	handler := f.handler
	if handler == nil {
		handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("ok"))
		})
	}

	ln, err := net.Listen("unix", req.SocketPath)
	if err != nil {
		return nil, err
	}
	srv := &http.Server{Handler: handler}
	go func() { _ = srv.Serve(ln) }()
	proc.stop = func() {
		_ = srv.Close()
		_ = os.Remove(req.SocketPath)
	}

	switch {
	case f.hangForever:
		// Never report; the host must time out and kill.
	case f.failReady != "":
		writeReady(req.ReadyFile, readyMsg{OK: false, PID: proc.pid, Error: f.failReady})
	default:
		writeReady(req.ReadyFile, readyMsg{OK: true, PID: proc.pid})
	}

	f.record(proc)

	// Held open at the very end of Spawn: the child is fully constructed and
	// serving, but load has not published it yet. That is the window an Evict
	// must not fall through — and it is only meaningful once there is a real
	// process to tear down.
	if f.gate != nil {
		if f.spawnEntered != nil {
			f.enteredOnce.Do(func() { close(f.spawnEntered) })
		}
		<-f.gate
	}

	return proc, nil
}

func (f *fakeSpawner) record(p *fakeProcess) {
	f.mu.Lock()
	f.procs = append(f.procs, p)
	f.mu.Unlock()
}

func writeReady(f *os.File, msg readyMsg) {
	body, _ := json.Marshal(msg)
	_, _ = f.Write(append(body, '\n'))
	_ = f.Close()
}

type fakeProcess struct {
	pid           int
	exited        chan struct{}
	exitOnce      sync.Once
	exitErr       error
	ignoreSIGTERM bool
	stop          func()
	gotSIGTERM    atomic.Bool
	killed        atomic.Bool
}

func (p *fakeProcess) Pid() int { return p.pid }

func (p *fakeProcess) Signal(sig os.Signal) error {
	p.gotSIGTERM.Store(true)
	if p.ignoreSIGTERM {
		return nil
	}
	p.die(nil)
	return nil
}

func (p *fakeProcess) Kill() error {
	p.killed.Store(true)
	p.die(fmt.Errorf("signal: killed"))
	return nil
}

func (p *fakeProcess) Wait() error {
	<-p.exited
	return p.exitErr
}

// crash simulates the tenant dying on its own (panic, OOM, JS fault).
func (p *fakeProcess) crash() { p.die(fmt.Errorf("exit status 2")) }

func (p *fakeProcess) die(err error) {
	p.exitOnce.Do(func() {
		if p.stop != nil {
			p.stop()
		}
		p.exitErr = err
		close(p.exited)
	})
}

// waitFor polls until cond is true or the deadline passes. Used instead of a
// fixed sleep so tests stay fast and non-flaky.
func waitFor(timeout time.Duration, cond func() bool) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(time.Millisecond)
	}
	return cond()
}
