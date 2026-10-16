package commands

import (
	"os"
	"runtime"
	"strings"
	"testing"

	"github.com/jfrog/jfrog-cli-core/v2/utils/config"
	utilsTests "github.com/jfrog/jfrog-cli-core/v2/utils/tests"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestAddCommandCredentialsNoSecretsInArgv checks that the password or token never appears in the
// curl argv (readable by other local users via ps or /proc/<pid>/cmdline), and is instead passed
// in a 0600 config file referenced by -K.
func TestAddCommandCredentialsNoSecretsInArgv(t *testing.T) {
	tests := []struct {
		name           string
		details        *config.ServerDetails
		secretMarkers  []string
		expectHelp     string
		expectConfigIn string
	}{
		{
			name: "userPassword",
			details: &config.ServerDetails{
				User: "admin",
				// jfrog-ignore - dummy password for tests
				Password: "s3cret-p@ss\"w\\ord",
			},
			secretMarkers:  []string{"s3cret-p@ss", "s3cret-p@ss\"w\\ord"},
			expectHelp:     "-K ***",
			expectConfigIn: `user = "admin:s3cret-p@ss\"w\\ord"`,
		},
		{
			name: "userOnly",
			details: &config.ServerDetails{
				User: "ad\"min\\",
			},
			secretMarkers:  []string{"ad\"min"},
			expectHelp:     "-K ***",
			expectConfigIn: `user = "ad\"min\\:"`,
		},
		{
			name: "passwordOnly",
			details: &config.ServerDetails{
				// jfrog-ignore - dummy password for tests
				Password: "only-password",
			},
			secretMarkers:  []string{"only-password"},
			expectHelp:     "-K ***",
			expectConfigIn: `user = ":only-password"`,
		},
		{
			name: "passwordWithTrailingLineBreak",
			details: &config.ServerDetails{
				User: "admin",
				// jfrog-ignore - dummy password for tests
				Password: "pasted-password\r\n",
			},
			secretMarkers:  []string{"pasted-password"},
			expectHelp:     "-K ***",
			expectConfigIn: "user = \"admin:pasted-password\"\n",
		},
		{
			name: "accessToken",
			//#nosec G101 jfrog-ignore - Dummy token for tests.
			details: &config.ServerDetails{
				AccessToken: "eyJver=1.tokenPart2.tokenPart3",
			},
			secretMarkers:  []string{"eyJver=1.tokenPart2.tokenPart3"},
			expectHelp:     "-K ***",
			expectConfigIn: `header = "Authorization: Bearer eyJver=1.tokenPart2.tokenPart3"`,
		},
		{
			name: "accessTokenWithQuoteAndBackslash",
			//#nosec G101 jfrog-ignore - Dummy token for tests.
			details: &config.ServerDetails{
				AccessToken: `tok"en\part`,
			},
			secretMarkers:  []string{`tok"en`},
			expectHelp:     "-K ***",
			expectConfigIn: `header = "Authorization: Bearer tok\"en\\part"`,
		},
		{
			name: "certificateAndCredentials",
			details: &config.ServerDetails{
				User: "admin",
				// jfrog-ignore - dummy password for tests
				Password:          "cert-password",
				ClientCertPath:    "/etc/ssl/certs/client.pem",
				ClientCertKeyPath: "/etc/ssl/private/client.key",
			},
			secretMarkers:  []string{"cert-password"},
			expectHelp:     "--cert *** --key *** -K ***",
			expectConfigIn: `user = "admin:cert-password"`,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cmd := &CurlCommand{
				executablePath: "curl",
				serverDetails:  tc.details,
				arguments:      []string{"-X", "GET", "api/system/ping"},
			}

			help, err := cmd.addCommandCredentials()
			require.NoError(t, err)
			defer cmd.cleanupCredentialsConfig()

			assert.Equal(t, tc.expectHelp, help,
				"debug help text should redact the secret")

			execCmd := cmd.GetCmd()
			joinedArgv := strings.Join(execCmd.Args, " ")
			for _, secret := range tc.secretMarkers {
				assert.NotContains(t, joinedArgv, secret,
					"secret %q must not appear in argv (would be readable via /proc/<pid>/cmdline)", secret)
			}

			// Credential must be delivered via -K <path>, and the path must exist and be readable only by us.
			kIdx := -1
			for i, a := range execCmd.Args {
				if a == "-K" {
					kIdx = i
					break
				}
			}
			require.NotEqual(t, -1, kIdx, "expected `-K` to be appended to argv so curl reads credentials from a config file")
			require.Less(t, kIdx+1, len(execCmd.Args), "-K must be followed by a path")
			configPath := execCmd.Args[kIdx+1]
			assert.NotEmpty(t, configPath)

			body, err := os.ReadFile(configPath)
			require.NoError(t, err, "curl credentials config file should exist while the command runs")
			assert.Contains(t, string(body), tc.expectConfigIn,
				"credentials config content should carry the expected curl-config line")

			if runtime.GOOS != "windows" {
				info, err := os.Stat(configPath)
				require.NoError(t, err)
				mode := info.Mode().Perm()
				assert.Equal(t, os.FileMode(0600), mode,
					"credentials config file must be created with 0600 mode so co-resident users cannot read it (got %o)", mode)
			}
		})
	}
}

// TestAddCommandCredentialsRejectsInnerLineBreak checks that a credential with a line break
// inside it is rejected, since curl can't read it from its config file.
func TestAddCommandCredentialsRejectsInnerLineBreak(t *testing.T) {
	cmd := &CurlCommand{
		executablePath: "curl",
		// jfrog-ignore - dummy password for tests
		serverDetails: &config.ServerDetails{User: "admin", Password: "pass\nword"},
		arguments:     []string{"-X", "GET", "api/system/ping"},
	}
	_, err := cmd.addCommandCredentials()
	assert.ErrorContains(t, err, "line break")
	assert.Empty(t, cmd.credConfigPath)
	assert.NotContains(t, cmd.arguments, "-K")
}

// TestAddCommandCredentialsCleanupRemovesFile checks that cleanup deletes the credentials config
// file, and that a second cleanup is a no-op.
func TestAddCommandCredentialsCleanupRemovesFile(t *testing.T) {
	cmd := &CurlCommand{
		executablePath: "curl",
		serverDetails:  &config.ServerDetails{User: "u", Password: "p"},
		arguments:      []string{"-X", "GET", "api/system/ping"},
	}
	_, err := cmd.addCommandCredentials()
	require.NoError(t, err)
	path := cmd.credConfigPath
	require.NotEmpty(t, path)
	_, err = os.Stat(path)
	require.NoError(t, err)

	cmd.cleanupCredentialsConfig()

	_, err = os.Stat(path)
	assert.True(t, os.IsNotExist(err), "credentials config file must be removed after Run exits")
	assert.Empty(t, cmd.credConfigPath)

	cmd.cleanupCredentialsConfig()
	assert.Empty(t, cmd.credConfigPath)
}

// TestGetServerDetailsExcludesRefreshableToken checks that a server with a password and a
// refreshable access token resolves to the password. jf refreshes tokens only for its own HTTP
// requests, so curl would otherwise get an expired token.
func TestGetServerDetailsExcludesRefreshableToken(t *testing.T) {
	cleanUp, err := utilsTests.SetJfrogHome()
	require.NoError(t, err)
	defer cleanUp()
	require.NoError(t, config.SaveServersConf([]*config.ServerDetails{{
		ServerId:       "refreshable",
		ArtifactoryUrl: "https://acme.jfrog.io/artifactory/",
		User:           "admin",
		// jfrog-ignore - dummy password for tests
		Password: "long-lived-password",
		//#nosec G101 jfrog-ignore - Dummy token for tests.
		AccessToken:             "expiring-token",
		ArtifactoryRefreshToken: "refresh-token",
	}}))

	cmd := NewCurlCommand().SetArguments([]string{"-X", "GET", "--server-id=refreshable", "api/system/ping"})
	details, err := cmd.GetServerDetails()
	require.NoError(t, err)
	assert.Empty(t, details.AccessToken)
	assert.Equal(t, "long-lived-password", details.Password)
}

// TestAddCommandCredentialsNoCredentials confirms that when the server has
// neither a token nor a user/password, no config file is written and no `-K`
// flag is added.
func TestAddCommandCredentialsNoCredentials(t *testing.T) {
	cmd := &CurlCommand{
		executablePath: "curl",
		serverDetails:  &config.ServerDetails{ClientCertPath: "/etc/ssl/certs/client.pem", ClientCertKeyPath: "/etc/ssl/private/client.key"},
		arguments:      []string{"-X", "GET", "api/system/ping"},
	}
	help, err := cmd.addCommandCredentials()
	require.NoError(t, err)
	defer cmd.cleanupCredentialsConfig()

	assert.Empty(t, cmd.credConfigPath, "no credentials -> no temp file")
	assert.Equal(t, "--cert *** --key *** ", help)
	joinedArgv := strings.Join(cmd.arguments, " ")
	assert.NotContains(t, joinedArgv, "-K",
		"no credentials means no -K flag should be added")
	// Cert path (not a secret) is still fine on argv.
	assert.Contains(t, joinedArgv, "--cert /etc/ssl/certs/client.pem")
}

func TestEscapeCurlConfigValue(t *testing.T) {
	assert.Equal(t, `plain`, escapeCurlConfigValue(`plain`))
	assert.Equal(t, `has \"quote\"`, escapeCurlConfigValue(`has "quote"`))
	assert.Equal(t, `back\\slash`, escapeCurlConfigValue(`back\slash`))
	assert.Equal(t, `both\\ and \"`, escapeCurlConfigValue(`both\ and "`))
}

func TestFindNextArg(t *testing.T) {
	command := &CurlCommand{}
	args := [][]string{
		{"-X", "GET", "arg1", "--foo", "bar"},
		{"-X", "GET", "--server-idea", "foo", "/api/arg2"},
		{"-XGET", "--foo", "bar", "--foo-bar", "meow", "arg3"},
	}

	expected := []struct {
		int
		string
	}{
		{2, "arg1"},
		{4, "/api/arg2"},
		{5, "arg3"},
	}

	for index, test := range args {
		command.arguments = test
		actualArgIndex, actualArg := command.findUriValueAndIndex()

		if actualArgIndex != expected[index].int {
			t.Errorf("Expected arg index of: %d, got: %d.", expected[index].int, actualArgIndex)
		}
		if actualArg != expected[index].string {
			t.Errorf("Expected arg index of: %s, got: %s.", expected[index].string, actualArg)
		}
	}
}

func TestIsCredsFlagExists(t *testing.T) {
	command := &CurlCommand{}
	args := [][]string{
		{"-X", "GET", "arg1", "--foo", "bar", "-uaaa:ppp"},
		{"-X", "GET", "--server-idea", "foo", "-u", "aaa:ppp", "/api/arg2"},
		{"-XGET", "--foo", "bar", "--foo-bar", "--user", "meow", "-Ttest"},
		{"-XGET", "--foo", "bar", "--foo-bar", "-Ttest"},
	}

	expected := []bool{
		true,
		true,
		true,
		false,
	}

	for index, test := range args {
		command.arguments = test
		flagExists := command.isCredentialsFlagExists()

		if flagExists != expected[index] {
			t.Errorf("Expected flag existstence to be: %t, got: %t.", expected[index], flagExists)
		}
	}
}

func TestBuildCommandUrl(t *testing.T) {
	tests := []struct {
		name      string
		arguments []string
		uriIndex  int
		uriValue  string
		expectErr bool
	}{
		{"basicGetWithLeadingSlash", []string{"-X", "GET", "/api/build/test1", "--server-id", "test1", "--foo", "bar"}, 2, "https://artifactory:8081/artifactory/api/build/test1", false},
		{"getWithInlineServerId", []string{"-X", "GET", "/api/build/test2", "--server-idea", "foo", "--server-id=test2"}, 2, "https://artifactory:8081/artifactory/api/build/test2", false},
		{"uriStartsWithDashDash", []string{"-XGET", "--/api/build/test3", "--server-id="}, 1, "https://artifactory:8081/artifactory/api/build/test3", true},
		{"uriStartsWithDash", []string{"-XGET", "-Test4", "--server-id", "bar"}, 1, "https://artifactory:8081/artifactory/api/build/test4", true},
		{"basicGetWithoutLeadingSlash", []string{"-X", "GET", "api/build/test5", "--server-id", "test5", "--foo", "bar"}, 2, "https://artifactory:8081/artifactory/api/build/test5", false},
		{"fullHTTP", []string{"-L", "http://example.com/api/test"}, -1, "", true},
		{"fullHTTPS", []string{"-L", "https://example.com/api/test"}, -1, "", true},
		{"noURI", []string{"-X", "GET", "-H", "Auth: token"}, -1, "", true},
		{"onlyFlags", []string{"-L", "-v", "-s", "--insecure"}, -1, "", true},
		{"specialChars", []string{"-X", "GET", "/api/test?param=value&foo=bar"}, 2, "https://artifactory:8081/artifactory/api/test?param=value&foo=bar", false},
		{"uriWithSpaces", []string{"-X", "GET", "/api/test%20space"}, 2, "https://artifactory:8081/artifactory/api/test%20space", false},
		{"multipleURIs", []string{"api/first", "-X", "GET", "api/second"}, 0, "https://artifactory:8081/artifactory/api/first", false},
		{"dashInURI", []string{"-X", "GET", "-not-a-flag/api/test"}, -1, "", true},
		{"booleanFlags", []string{"-L", "-k", "-v", "-s", "api/test"}, 4, "https://artifactory:8081/artifactory/api/test", false},
		{"longBooleanFlags", []string{"--location", "--insecure", "--verbose", "api/test"}, 3, "https://artifactory:8081/artifactory/api/test", false},
		{"embeddedValue", []string{"--header=Authorization: Bearer token", "-XPOST", "api/test"}, 2, "https://artifactory:8081/artifactory/api/test", false},
		{"multipleEmbedded", []string{"--data=json", "--header=Content-Type: application/json", "api/test"}, 2, "https://artifactory:8081/artifactory/api/test", false},
		{"combinedBoolean", []string{"-Lkvs", "api/test"}, 1, "https://artifactory:8081/artifactory/api/test", false},
		{"combinedMixed", []string{"-LvX", "POST", "api/test"}, 2, "https://artifactory:8081/artifactory/api/test", false},
		{"trailingValueFlag", []string{"api/test", "-X"}, 0, "https://artifactory:8081/artifactory/api/test", false},
		{"trailingBooleanFlag", []string{"api/test", "-L"}, 0, "https://artifactory:8081/artifactory/api/test", false},
		{"emptyArgs", []string{"-X", "GET", "", "api/test"}, 2, "https://artifactory:8081/artifactory/", false},
		{"emptyFlagValue", []string{"-H", "", "api/test"}, 2, "https://artifactory:8081/artifactory/api/test", false},
		{"uriFirst", []string{"api/test", "-X", "GET", "-L"}, 0, "https://artifactory:8081/artifactory/api/test", false},
		{"realWorld", []string{"-sS", "-L", "-X", "POST", "-H", "Content-Type: application/json", "-d", `{"key":"value"}`, "--insecure", "api/repos/test"}, 9, "https://artifactory:8081/artifactory/api/repos/test", false},
	}

	command := &CurlCommand{}
	urlPrefix := "https://artifactory:8081/artifactory/"
	for _, test := range tests {
		command.arguments = test.arguments
		t.Run(test.name, func(t *testing.T) {
			uriIndex, uriValue, err := command.buildCommandUrl(urlPrefix)

			// Check errors.
			if err != nil && !test.expectErr {
				t.Error(err)
			}
			if err == nil && test.expectErr {
				t.Errorf("Expecting: error, Got: nil")
			}

			if err == nil {
				// Validate results.
				if uriValue != test.uriValue {
					t.Errorf("Expected uri value of: %s, got: %s.", test.uriValue, uriValue)
				}
				if uriIndex != test.uriIndex {
					t.Errorf("Expected uri index of: %d, got: %d.", test.uriIndex, uriIndex)
				}
			}
		})
	}
}

func TestFindUriWithBooleanFlags(t *testing.T) {
	tests := []struct {
		name             string
		arguments        []string
		expectedUriIndex int
		expectedUri      string
	}{
		{
			name:             "shortSilentWithLongVerbose",
			arguments:        []string{"-s", "--show-error", "api/repositories/dev-master-maven-local", "--verbose"},
			expectedUriIndex: 2,
			expectedUri:      "api/repositories/dev-master-maven-local",
		},
		{
			name:             "outputFlagWithCombinedVerbose",
			arguments:        []string{"-o", "helm.tar.gz", "-L", "-vvv", "helm-sh/helm-v3.19.0-linux-amd64.tar.gz"},
			expectedUriIndex: 4,
			expectedUri:      "helm-sh/helm-v3.19.0-linux-amd64.tar.gz",
		},
		{
			name:             "combinedVerboseBeforeLocation",
			arguments:        []string{"-o", "helm.tar.gz", "-vvv", "-L", "helm-sh/helm-v3.19.0-linux-amd64.tar.gz"},
			expectedUriIndex: 4,
			expectedUri:      "helm-sh/helm-v3.19.0-linux-amd64.tar.gz",
		},
		{
			name:             "outputWithLocationAndSilent",
			arguments:        []string{"-o", "helm.tar.gz", "-L", "-s", "helm-sh/helm-v3.19.0-linux-amd64.tar.gz"},
			expectedUriIndex: 4,
			expectedUri:      "helm-sh/helm-v3.19.0-linux-amd64.tar.gz",
		},
		{
			name:             "locationFirstThenOutput",
			arguments:        []string{"-L", "-o", "helm.tar.gz", "helm-sh/helm-v3.19.0-linux-amd64.tar.gz"},
			expectedUriIndex: 3,
			expectedUri:      "helm-sh/helm-v3.19.0-linux-amd64.tar.gz",
		},
		{
			name:             "outputThenLocationSimple",
			arguments:        []string{"-o", "helm.tar.gz", "-L", "helm-sh/helm-v3.19.0-linux-amd64.tar.gz"},
			expectedUriIndex: 3,
			expectedUri:      "helm-sh/helm-v3.19.0-linux-amd64.tar.gz",
		},
		{
			name:             "locationCombinedVerboseThenOutput",
			arguments:        []string{"-L", "-vvv", "-o", "helm.tar.gz", "helm-sh/helm-v3.19.0-linux-amd64.tar.gz"},
			expectedUriIndex: 4,
			expectedUri:      "helm-sh/helm-v3.19.0-linux-amd64.tar.gz",
		},
		{
			name:             "locationOutputThenCombinedVerbose",
			arguments:        []string{"-L", "-o", "helm.tar.gz", "-vvv", "helm-sh/helm-v3.19.0-linux-amd64.tar.gz"},
			expectedUriIndex: 4,
			expectedUri:      "helm-sh/helm-v3.19.0-linux-amd64.tar.gz",
		},
		{
			name:             "combinedSilentShowErrorWithLocation",
			arguments:        []string{"-sS", "-L", "api/system/ping"},
			expectedUriIndex: 2,
			expectedUri:      "api/system/ping",
		},
		{
			name:             "longFormSilentShowErrorLocation",
			arguments:        []string{"--silent", "--show-error", "--location", "api/system/ping"},
			expectedUriIndex: 3,
			expectedUri:      "api/system/ping",
		},
		{
			name:             "getWithHeaderAndBooleanFlags",
			arguments:        []string{"-X", "GET", "-H", "Content-Type: application/json", "--verbose", "--insecure", "api/repositories"},
			expectedUriIndex: 6,
			expectedUri:      "api/repositories",
		},
		{
			name:             "inlineRequestAndHeaderWithLocation",
			arguments:        []string{"-XPOST", "-HContent-Type:application/json", "-L", "api/repositories"},
			expectedUriIndex: 3,
			expectedUri:      "api/repositories",
		},
		{
			name:             "longFormInlineRequestAndHeader",
			arguments:        []string{"--request=GET", "--header=Accept:application/json", "-v", "api/system/ping"},
			expectedUriIndex: 3,
			expectedUri:      "api/system/ping",
		},
		{
			name:             "allShortBooleanFlags",
			arguments:        []string{"-#", "-0", "-1", "-2", "-3", "-4", "-6", "-a", "-B", "-f", "-g", "-G", "-I", "-i", "api/test"},
			expectedUriIndex: 14,
			expectedUri:      "api/test",
		},
		{
			name:             "mixedKnownUnknownFlags",
			arguments:        []string{"-L", "-9", "possibleValue", "api/test"},
			expectedUriIndex: 3,
			expectedUri:      "api/test",
		},
		{
			name:             "complexCombinedFlags",
			arguments:        []string{"-sLkvo", "output.txt", "api/test"},
			expectedUriIndex: 2,
			expectedUri:      "api/test",
		},
		{
			name:             "httpMethodsAsValues",
			arguments:        []string{"-X", "PATCH", "-X", "OPTIONS", "-X", "TRACE", "api/test"},
			expectedUriIndex: 6,
			expectedUri:      "api/test",
		},
		{
			name:             "quotedValues",
			arguments:        []string{"-H", `Authorization: "Bearer token"`, "-H", "Accept: */*", "api/test"},
			expectedUriIndex: 4,
			expectedUri:      "api/test",
		},
		{
			name:             "jsonData",
			arguments:        []string{"-d", `{"test": "value", "array": [1,2,3]}`, "-H", "Content-Type: application/json", "api/test"},
			expectedUriIndex: 4,
			expectedUri:      "api/test",
		},
		{
			name:             "fileReference",
			arguments:        []string{"-d", "@/path/to/file.json", "-T", "/path/to/upload.tar", "api/test"},
			expectedUriIndex: 4,
			expectedUri:      "api/test",
		},
		{
			name:             "repeatedBooleanFlags",
			arguments:        []string{"-v", "-v", "-v", "-L", "-L", "api/test"},
			expectedUriIndex: 5,
			expectedUri:      "api/test",
		},
		{
			name:             "urlEncodedData",
			arguments:        []string{"--data-urlencode", "param=value&other=test", "api/test"},
			expectedUriIndex: 2,
			expectedUri:      "api/test",
		},
		{
			name:             "proxySettings",
			arguments:        []string{"-x", "proxy.server:8080", "-U", "proxyuser:pass", "api/test"},
			expectedUriIndex: 4,
			expectedUri:      "api/test",
		},
		{
			name:             "certAndKeyFlags",
			arguments:        []string{"-E", "/path/to/cert.pem", "--key", "/path/to/key.pem", "api/test"},
			expectedUriIndex: 4,
			expectedUri:      "api/test",
		},
		{
			name:             "rangeHeader",
			arguments:        []string{"-r", "0-1023", "-C", "-", "api/download/file"},
			expectedUriIndex: 4,
			expectedUri:      "api/download/file",
		},
		{
			name:             "userAgentAndReferer",
			arguments:        []string{"-A", "CustomUserAgent/1.0", "-e", "https://referrer.com", "api/test"},
			expectedUriIndex: 4,
			expectedUri:      "api/test",
		},
		{
			name:             "formData",
			arguments:        []string{"-F", "field1=value1", "-F", "field2=@file.txt", "-F", "field3=<file2.txt", "api/upload"},
			expectedUriIndex: 6,
			expectedUri:      "api/upload",
		},
		{
			name:             "cookiesAndJar",
			arguments:        []string{"-b", "cookies.txt", "-c", "newcookies.txt", "-b", "name=value", "api/test"},
			expectedUriIndex: 6,
			expectedUri:      "api/test",
		},
		{
			name:             "speedAndTime",
			arguments:        []string{"-Y", "1000", "-y", "30", "-m", "120", "api/test"},
			expectedUriIndex: 6,
			expectedUri:      "api/test",
		},
		{
			name:             "retryOptions",
			arguments:        []string{"--retry", "3", "--retry-delay", "5", "--retry-max-time", "60", "api/test"},
			expectedUriIndex: 6,
			expectedUri:      "api/test",
		},
		{
			name:             "writeOutFormat",
			arguments:        []string{"-w", `%{http_code}\n`, "-o", "/dev/null", "-s", "api/test"},
			expectedUriIndex: 5,
			expectedUri:      "api/test",
		},
		{
			name:             "ipv6Address",
			arguments:        []string{"-H", "Host: [::1]", "-6", "api/test"},
			expectedUriIndex: 3,
			expectedUri:      "api/test",
		},
		{
			name:             "traceAndDump",
			arguments:        []string{"--trace", "trace.txt", "--trace-ascii", "ascii.txt", "--dump-header", "headers.txt", "api/test"},
			expectedUriIndex: 6,
			expectedUri:      "api/test",
		},
	}

	command := &CurlCommand{}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			command.arguments = test.arguments
			actualIndex, actualUri := command.findUriValueAndIndex()

			if actualIndex != test.expectedUriIndex {
				t.Errorf("Expected URI index: %d, got: %d. Arguments: %v", test.expectedUriIndex, actualIndex, test.arguments)
			}
			if actualUri != test.expectedUri {
				t.Errorf("Expected URI: %s, got: %s. Arguments: %v", test.expectedUri, actualUri, test.arguments)
			}
		})
	}
}
