package testutil

import (
	"errors"
	"fmt"
	"math/rand"
	"os"
	"os/exec"
	"time"

	"github.com/hashicorp/nomad/helper/freeport"
	"github.com/hashicorp/nomad/helper/testlog"
	"github.com/hashicorp/nomad/helper/uuid"
	"github.com/hashicorp/nomad/nomad/structs/config"
	vapi "github.com/hashicorp/vault/api"
	testing "github.com/mitchellh/go-testing-interface"
	"github.com/stretchr/testify/require"
)

// TestVault is a test helper. It uses a fork/exec model to create a test Vault
// server instance in the background and can be initialized with policies, roles
// and backends mounted. The test Vault instances can be used to run a unit test
// and offers and easy API to tear itself down on test end. The only
// prerequisite is that the Vault binary is on the $PATH.

// TestVault wraps a test Vault server launched in dev mode, suitable for
// testing.
type TestVault struct {
	cmd    *exec.Cmd
	t      testing.T
	waitCh chan error

	// ports (if any) that are reserved through freeport that must be returned
	// at the end of a test, done when Stop() is called.
	ports []int

	Addr      string
	HTTPAddr  string
	RootToken string
	Config    *config.VaultConfig
	Client    *vapi.Client
}

func NewTestVaultFromPath(t testing.T, binary string) *TestVault {
	var ports []int
	nextPort := func() int {
		next := freeport.MustTake(1)
		ports = append(ports, next...)
		return next[0]
	}

	for i := 10; i >= 0; i-- {

		port := nextPort() // collect every port for cleanup after the test

		token := uuid.Generate()
		bind := fmt.Sprintf("-dev-listen-address=127.0.0.1:%d", port)
		http := fmt.Sprintf("http://127.0.0.1:%d", port)
		root := fmt.Sprintf("-dev-root-token-id=%s", token)

		cmd := exec.Command(binary, "server", "-dev", bind, root)
		cmd.Stdout = testlog.NewWriter(t)
		cmd.Stderr = testlog.NewWriter(t)

		// Build the config
		conf := vapi.DefaultConfig()
		conf.Address = http

		// Make the client and set the token to the root token
		client, err := vapi.NewClient(conf)
		if err != nil {
			t.Fatalf("failed to build Vault API client: %v", err)
		}
		client.SetToken(token)

		enable := true
		tv := &TestVault{
			cmd:       cmd,
			t:         t,
			ports:     ports,
			Addr:      bind,
			HTTPAddr:  http,
			RootToken: token,
			Client:    client,
			Config: &config.VaultConfig{
				Enabled: &enable,
				Token:   token,
				Addr:    http,
			},
		}

		if err := tv.cmd.Start(); err != nil {
			tv.t.Fatalf("failed to start vault: %v", err)
		}

		// Start the waiter
		tv.waitCh = make(chan error, 1)
		go func() {
			err := tv.cmd.Wait()
			tv.waitCh <- err
		}()

		// Ensure Vault started
		var startErr error
		select {
		case startErr = <-tv.waitCh:
		case <-time.After(time.Duration(500*TestMultiplier()) * time.Millisecond):
		}

		if startErr != nil && i == 0 {
			t.Fatalf("failed to start vault: %v", startErr)
		} else if startErr != nil {
			wait := time.Duration(rand.Int31n(2000)) * time.Millisecond
			time.Sleep(wait)
			continue
		}

		waitErr := tv.waitForAPI()
		if waitErr != nil && i == 0 {
			t.Fatalf("failed to start vault: %v", waitErr)
		} else if waitErr != nil {
			wait := time.Duration(rand.Int31n(2000)) * time.Millisecond
			time.Sleep(wait)
			continue
		}

		return tv
	}

	return nil

}

// NewTestVault returns a new TestVault instance that is ready for API calls
func NewTestVault(t testing.T) *TestVault {
	// Lookup vault from the path
	return NewTestVaultFromPath(t, "vault")
}

// NewTestVaultDelayed returns a test Vault server that has not been started.
// Start must be called and it is the callers responsibility to deal with any
// port conflicts that may occur and retry accordingly.
func NewTestVaultDelayed(t testing.T) *TestVault {
	port := freeport.MustTake(1)[0]
	token := uuid.Generate()
	bind := fmt.Sprintf("-dev-listen-address=127.0.0.1:%d", port)
	http := fmt.Sprintf("http://127.0.0.1:%d", port)
	root := fmt.Sprintf("-dev-root-token-id=%s", token)

	cmd := exec.Command("vault", "server", "-dev", bind, root)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	// Build the config
	conf := vapi.DefaultConfig()
	conf.Address = http

	// Make the client and set the token to the root token
	client, err := vapi.NewClient(conf)
	if err != nil {
		t.Fatalf("failed to build Vault API client: %v", err)
	}
	client.SetToken(token)

	enable := true
	tv := &TestVault{
		cmd:       cmd,
		t:         t,
		Addr:      bind,
		HTTPAddr:  http,
		RootToken: token,
		Client:    client,
		Config: &config.VaultConfig{
			Enabled: &enable,
			Token:   token,
			Addr:    http,
		},
	}

	return tv
}

// Start starts the test Vault server and waits for it to respond to its HTTP
// API
func (tv *TestVault) Start() error {
	// Start the waiter
	tv.waitCh = make(chan error, 1)

	go func() {
		// Must call Start and Wait in the same goroutine on Windows #5174
		if err := tv.cmd.Start(); err != nil {
			tv.waitCh <- err
			return
		}

		err := tv.cmd.Wait()
		tv.waitCh <- err
	}()

	// Ensure Vault started
	select {
	case err := <-tv.waitCh:
		return err
	case <-time.After(time.Duration(500*TestMultiplier()) * time.Millisecond):
	}

	return tv.waitForAPI()
}

// Stop stops the test Vault server
func (tv *TestVault) Stop() {
	defer freeport.Return(tv.ports)

	if tv.cmd.Process == nil {
		return
	}

	if err := tv.cmd.Process.Kill(); err != nil {
		if errors.Is(err, os.ErrProcessDone) {
			return
		}
		tv.t.Errorf("err: %s", err)
	}
	if tv.waitCh != nil {
		select {
		case <-tv.waitCh:
			return
		case <-time.After(1 * time.Second):
			require.Fail(tv.t, "Timed out waiting for vault to terminate")
		}
	}
}

// waitForAPI waits for the Vault HTTP endpoint to start
// responding. This is an indication that the agent has started.
func (tv *TestVault) waitForAPI() error {
	var waitErr error
	WaitForResult(func() (bool, error) {
		inited, err := tv.Client.Sys().InitStatus()
		if err != nil {
			return false, err
		}
		return inited, nil
	}, func(err error) {
		waitErr = err
	})
	return waitErr
}

// VaultVersion returns the Vault version as a string or an error if it couldn't
// be determined
func VaultVersion() (string, error) {
	cmd := exec.Command("vault", "version")
	out, err := cmd.Output()
	return string(out), err
}
