package integrationTests

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

const (
	AgentBin                = "../agent/build/mizuagent"
	BaseninePort            = "9099"
	BasenineCommandTimeout  = 5 * time.Second
	APIServerCommandTimeout = 10 * time.Second
	TestTimeout             = 60 * time.Second
	PCAPFile                = "http.cap"
)

func fileExists(path string) bool {
	_, err := os.Stat(path)
	if err == nil {
		return true
	}
	if errors.Is(err, os.ErrNotExist) {
		return false
	}
	return false
}

func cleanupCommand(cmd *exec.Cmd) error {
	if err := cmd.Process.Signal(syscall.SIGQUIT); err != nil {
		return err
	}

	if err := cmd.Wait(); err != nil {
		return err
	}
	return nil
}

func startBasenine(t *testing.T) (*exec.Cmd, string) {
	ctx, _ := context.WithTimeout(context.Background(), TestTimeout)
	basenineCmd := exec.CommandContext(ctx, "basenine", "-port", BaseninePort)
	t.Logf("running command: %v\n", basenineCmd.String())

	t.Cleanup(func() {
		if err := cleanupCommand(basenineCmd); err != nil {
			t.Logf("failed to cleanup basenine command, err: %v", err)
		}
	})

	// basenine outputs only to stderr
	out, err := basenineCmd.StderrPipe()
	if err != nil {
		t.Errorf("failed to pipe basenine command output: %v", err)
		return nil, ""
	}

	if err := basenineCmd.Start(); err != nil {
		t.Errorf("failed to start basenine command: %v", err)
		return nil, ""
	}

	// wait for some output
	buff := make([]byte, 64)
	for stay, timeout := true, time.After(BasenineCommandTimeout); stay; {
		if n, err := out.Read(buff); err == nil {
			return basenineCmd, string(buff[:n])
		}
		select {
		case <-timeout:
			stay = false
		default:
		}
	}

	t.Error("basenine command did not output any data in time")
	return nil, ""
}

func startAPIServer(t *testing.T, configPath string) (*exec.Cmd, io.ReadCloser, string) {
	args := []string{"--api-server"}
	if len(configPath) > 0 {
		args = append(args, "--config-path", configPath)
	}

	ctx, _ := context.WithTimeout(context.Background(), TestTimeout)
	apiServerCmd := exec.CommandContext(ctx, AgentBin, args...)
	t.Logf("running command: %v", apiServerCmd.String())

	t.Cleanup(func() {
		if err := cleanupCommand(apiServerCmd); err != nil {
			t.Logf("failed to cleanup API Server command, err: %v", err)
		}
	})

	out, err := apiServerCmd.StderrPipe()
	if err != nil {
		t.Errorf("failed to pipe API Server command output: %v", err)
		return nil, nil, ""
	}

	if err := apiServerCmd.Start(); err != nil {
		t.Errorf("failed to start API Server command: %v", err)
		return nil, nil, ""
	}

	// wait for some output
	buff := make([]byte, 32)
	for stay, timeout := true, time.After(APIServerCommandTimeout); stay; {
		if n, err := out.Read(buff); err == nil {
			return apiServerCmd, out, string(buff[:n])
		}
		select {
		case <-timeout:
			stay = false
		default:
		}
	}

	t.Error("API Server command did not output any data in time")
	return nil, nil, ""
}

func startTapper(t *testing.T, pcapPath string) (*exec.Cmd, io.ReadCloser, string) {
	if len(pcapPath) == 0 {
		t.Error("tapper PCAP file path is empty")
		return nil, nil, ""
	}

	if !fileExists(pcapPath) {
		t.Errorf("tapper PCAP file does not exist: %s", pcapPath)
		return nil, nil, ""
	}

	if !strings.HasSuffix(pcapPath, ".cap") {
		t.Errorf("tapper PCAP file is not a valid .cap file: %s", pcapPath)
		return nil, nil, ""
	}

	args := []string{"-r", pcapPath, "--tap", "--api-server-address", "ws://localhost:8899/wsTapper"}
	ctx, _ := context.WithTimeout(context.Background(), TestTimeout)
	tapperCmd := exec.CommandContext(ctx, AgentBin, args...)
	t.Logf("running command: %v", tapperCmd.String())

	t.Cleanup(func() {
		if err := cleanupCommand(tapperCmd); err != nil {
			t.Logf("failed to cleanup tapper command, err: %v", err)
		}
	})

	out, err := tapperCmd.StderrPipe()
	if err != nil {
		t.Errorf("failed to pipe tapper command output: %v", err)
		return nil, nil, ""
	}

	if err := tapperCmd.Start(); err != nil {
		t.Errorf("failed to start tapper command: %v", err)
		return nil, nil, ""
	}

	// wait for some output
	buff := make([]byte, 32)
	for stay, timeout := true, time.After(APIServerCommandTimeout); stay; {
		if n, err := out.Read(buff); err == nil {
			return tapperCmd, out, string(buff[:n])
		}
		select {
		case <-timeout:
			stay = false
		default:
		}
	}

	t.Error("tapper command did not output any data in time")
	return nil, nil, ""
}

func readOutput(output chan []byte, rc io.ReadCloser) {
	buff := make([]byte, 4096)
	for {
		n, err := rc.Read(buff)
		if err != nil {
			break
		}
		output <- buff[:n]
	}
}

func readTapperOutput(t *testing.T, wg *sync.WaitGroup, rc io.ReadCloser) {
	wg.Add(1)

	tapperOutputChan := make(chan []byte)
	go readOutput(tapperOutputChan, rc)
	tapperOutput := <-tapperOutputChan
	rc.Close()

	output := string(tapperOutput)
	t.Logf("tapper output: %s\n", output)

	if !strings.Contains(output, "Starting tapper, websocket address: ws://localhost:8899/wsTapper") {
		t.Error("failed to validate tapper output")
	}

	if !strings.Contains(output, fmt.Sprintf("Start reading packets from file-%s", PCAPFile)) {
		t.Error("failed to validate tapper output")
	}

	if !strings.Contains(output, fmt.Sprintf("Got EOF while reading packets from file-%s", PCAPFile)) {
		t.Error("failed to validate tapper output")
	}

	wg.Done()
}

func Test(t *testing.T) {
	if testing.Short() {
		t.Skip("ignored acceptance test")
	}

	expectedBasenineOutput := fmt.Sprintf("Listening on :%s\n", BaseninePort)
	expectedAgentOutput := "Initializing"

	_, basenineOutput := startBasenine(t)
	if !strings.HasSuffix(basenineOutput, expectedBasenineOutput) {
		t.Errorf("basenine is not running as expected - expected: %s, actual: %s", expectedBasenineOutput, basenineOutput)
	}

	_, tapperReader, tapperInit := startTapper(t, PCAPFile)
	if !strings.HasSuffix(tapperInit, expectedAgentOutput) {
		t.Errorf("Tapper is not running as expected - expected: %s, actual: %s", expectedAgentOutput, tapperInit)
	}

	_, _, apiServerInit := startAPIServer(t, "")
	if !strings.HasSuffix(apiServerInit, expectedAgentOutput) {
		t.Errorf("API Server is not running as expected - expected: %s, actual: %s", expectedAgentOutput, apiServerInit)
	}

	var wg = sync.WaitGroup{}

	readTapperOutput(t, &wg, tapperReader)

	wg.Wait()
}
