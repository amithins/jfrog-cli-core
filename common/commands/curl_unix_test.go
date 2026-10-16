//go:build !windows

package commands

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/jfrog/jfrog-cli-core/v2/utils/config"
	"github.com/jfrog/jfrog-client-go/utils/log"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestCurlRunKeepsSecretsOutOfArgvAndLogs runs the command against a stub curl that records its
// argv and copies the -K file. The secret must reach curl only through that file, never through
// argv or the debug log, and the file must be gone after curl exits, whether it succeeds or fails.
func TestCurlRunKeepsSecretsOutOfArgvAndLogs(t *testing.T) {
	// jfrog-ignore - dummy password for tests
	const password = "run-secret-password"
	for _, exitCode := range []string{"0", "1"} {
		t.Run("curl exit "+exitCode, func(t *testing.T) {
			stubDir := t.TempDir()
			argvLog := filepath.Join(stubDir, "argv.log")
			configCopy := filepath.Join(stubDir, "config.copy")
			stub := `#!/bin/sh
printf '%s\n' "$@" > "` + argvLog + `"
while [ $# -gt 0 ]; do
  if [ "$1" = "-K" ]; then cp "$2" "` + configCopy + `"; fi
  shift
done
exit ` + exitCode + "\n"
			require.NoError(t, os.WriteFile(filepath.Join(stubDir, "curl"), []byte(stub), 0700)) // #nosec G306 -- test stub must be executable
			t.Setenv("PATH", stubDir+string(os.PathListSeparator)+os.Getenv("PATH"))

			var output bytes.Buffer
			previousLogger := log.Logger
			log.SetLogger(log.NewLogger(log.DEBUG, &output))
			defer log.SetLogger(previousLogger)

			cmd := NewCurlCommand().
				SetServerDetails(&config.ServerDetails{User: "admin", Password: password}).
				SetUrl("https://acme.jfrog.io/artifactory/").
				SetArguments([]string{"-X", "GET", "api/system/ping"})
			err := cmd.Run()
			if exitCode == "0" {
				assert.NoError(t, err)
			} else {
				assert.Error(t, err)
			}

			argv, err := os.ReadFile(argvLog)
			require.NoError(t, err)
			assert.NotContains(t, string(argv), password)
			assert.NotContains(t, output.String(), password)
			assert.Contains(t, output.String(), "-K ***")

			copied, err := os.ReadFile(configCopy)
			require.NoError(t, err)
			assert.Equal(t, "user = \"admin:"+password+"\"\n", string(copied))

			args := strings.Split(strings.TrimSpace(string(argv)), "\n")
			require.GreaterOrEqual(t, len(args), 2)
			assert.Equal(t, "-K", args[len(args)-2])
			assert.NoFileExists(t, args[len(args)-1], "credentials config file must be removed after curl exits")
		})
	}
}

func TestAddCommandCredentialsTempDirUnwritable(t *testing.T) {
	t.Setenv("TMPDIR", filepath.Join(t.TempDir(), "missing"))
	cmd := &CurlCommand{
		executablePath: "curl",
		serverDetails:  &config.ServerDetails{User: "u", Password: "p"},
		arguments:      []string{"-X", "GET", "api/system/ping"},
	}
	_, err := cmd.addCommandCredentials()
	assert.Error(t, err)
	assert.Empty(t, cmd.credConfigPath)
	assert.NotContains(t, cmd.arguments, "-K")
}

// TestRemoveCredentialsConfigOnSignal checks that SIGTERM while curl runs removes the credentials
// config file before jf exits.
func TestRemoveCredentialsConfigOnSignal(t *testing.T) {
	cmd := &CurlCommand{
		executablePath: "curl",
		serverDetails:  &config.ServerDetails{User: "u", Password: "p"},
		arguments:      []string{"-X", "GET", "api/system/ping"},
	}
	_, err := cmd.addCommandCredentials()
	require.NoError(t, err)
	defer cmd.cleanupCredentialsConfig()
	path := cmd.credConfigPath

	exitCodes := make(chan int, 1)
	previousExit := exitOnSignal
	exitOnSignal = func(code int) { exitCodes <- code }
	defer func() { exitOnSignal = previousExit }()

	stop := cmd.removeCredentialsConfigOnSignal()
	defer stop()
	require.NoError(t, syscall.Kill(os.Getpid(), syscall.SIGTERM))

	select {
	case code := <-exitCodes:
		assert.Equal(t, 128+int(syscall.SIGTERM), code)
	case <-time.After(5 * time.Second):
		t.Fatal("signal handler did not run")
	}
	assert.NoFileExists(t, path)
}
