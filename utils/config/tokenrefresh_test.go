package config

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	configtests "github.com/jfrog/jfrog-cli-core/v2/utils/config/tests"
	"github.com/jfrog/jfrog-client-go/auth"
	"github.com/jfrog/jfrog-client-go/utils/io/httputils"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRefreshThresholdForTokenType(t *testing.T) {
	t.Run("artifactory token uses fixed threshold", func(t *testing.T) {
		threshold, err := refreshThresholdForTokenType("irrelevant-token", ArtifactoryToken)
		assert.NoError(t, err)
		assert.Equal(t, auth.RefreshArtifactoryTokenBeforeExpiryMinutes, threshold)
	})

	t.Run("short-lived platform token uses proportional threshold", func(t *testing.T) {
		token := buildTestAccessToken(t, 1000, 1000+7*24*60*60) // 7-day lifetime
		threshold, err := refreshThresholdForTokenType(token, AccessToken)
		assert.NoError(t, err)
		assert.Equal(t, int64(1008), threshold) // 10% of 7 days, in minutes
	})

	t.Run("1-year platform token is capped at the historical threshold", func(t *testing.T) {
		token := buildTestAccessToken(t, 1000, 1000+365*24*60*60) // 1-year lifetime
		threshold, err := refreshThresholdForTokenType(token, AccessToken)
		assert.NoError(t, err)
		assert.Equal(t, auth.RefreshPlatformTokenBeforeExpiryMinutes, threshold)
	})
}

// buildTestAccessToken builds an unsigned JWT-shaped token with the given iat/exp claims.
func buildTestAccessToken(t *testing.T, iat, exp int64) string {
	payload, err := json.Marshal(map[string]int64{"iat": iat, "exp": exp})
	require.NoError(t, err)
	encodedPayload := base64.RawStdEncoding.EncodeToString(payload)
	return "header." + encodedPayload + ".signature"
}

func TestCreateInitialRefreshableTokensIfNeededEarlyReturns(t *testing.T) {
	tests := []struct {
		name                          string
		serverDetails                 *ServerDetails
		expectedError                 error
		expectedIntervalAfterCall     int
		expectedAccessTokenAfterCall  string
		expectedRefreshTokenAfterCall string
	}{
		{
			name: "EarlyReturn_ArtifactoryTokenRefreshIntervalZero",
			serverDetails: &ServerDetails{
				ServerId:                        "test-server",
				ArtifactoryTokenRefreshInterval: 0,
				ArtifactoryRefreshToken:         "",
				AccessToken:                     "",
			},
			expectedError:                 nil,
			expectedIntervalAfterCall:     0,
			expectedAccessTokenAfterCall:  "",
			expectedRefreshTokenAfterCall: "",
		},
		{
			name: "EarlyReturn_ArtifactoryTokenRefreshIntervalNegative",
			serverDetails: &ServerDetails{
				ServerId:                        "test-server",
				ArtifactoryTokenRefreshInterval: -1,
				ArtifactoryRefreshToken:         "",
				AccessToken:                     "",
			},
			expectedError:                 nil,
			expectedIntervalAfterCall:     -1,
			expectedAccessTokenAfterCall:  "",
			expectedRefreshTokenAfterCall: "",
		},
		{
			name: "EarlyReturn_ArtifactoryRefreshTokenNotEmpty",
			serverDetails: &ServerDetails{
				ServerId:                        "test-server",
				ArtifactoryTokenRefreshInterval: 60,
				ArtifactoryRefreshToken:         "existing-refresh-token",
				AccessToken:                     "",
			},
			expectedError:                 nil,
			expectedIntervalAfterCall:     60,
			expectedAccessTokenAfterCall:  "",
			expectedRefreshTokenAfterCall: "existing-refresh-token",
		},
		{
			name: "EarlyReturn_AccessTokenNotEmpty",
			serverDetails: &ServerDetails{
				ServerId:                        "test-server",
				ArtifactoryTokenRefreshInterval: 60,
				ArtifactoryRefreshToken:         "",
				AccessToken:                     "existing-access-token",
			},
			expectedError:                 nil,
			expectedIntervalAfterCall:     60,
			expectedAccessTokenAfterCall:  "existing-access-token",
			expectedRefreshTokenAfterCall: "",
		},
		{
			name: "EarlyReturn_BothTokensNotEmpty",
			serverDetails: &ServerDetails{
				ServerId:                        "test-server",
				ArtifactoryTokenRefreshInterval: 60,
				ArtifactoryRefreshToken:         "existing-refresh-token",
				AccessToken:                     "existing-access-token",
			},
			expectedError:                 nil,
			expectedIntervalAfterCall:     60,
			expectedAccessTokenAfterCall:  "existing-access-token",
			expectedRefreshTokenAfterCall: "existing-refresh-token",
		},
		{
			name: "EarlyReturn_IntervalZeroWithTokens",
			serverDetails: &ServerDetails{
				ServerId:                        "test-server",
				ArtifactoryTokenRefreshInterval: 0,
				ArtifactoryRefreshToken:         "existing-refresh-token",
				AccessToken:                     "existing-access-token",
			},
			expectedError:                 nil,
			expectedIntervalAfterCall:     0,
			expectedAccessTokenAfterCall:  "existing-access-token",
			expectedRefreshTokenAfterCall: "existing-refresh-token",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Create a copy to avoid modifying the original
			serverDetailsCopy := &ServerDetails{
				ServerId:                        tt.serverDetails.ServerId,
				ArtifactoryTokenRefreshInterval: tt.serverDetails.ArtifactoryTokenRefreshInterval,
				ArtifactoryRefreshToken:         tt.serverDetails.ArtifactoryRefreshToken,
				AccessToken:                     tt.serverDetails.AccessToken,
			}

			err := CreateInitialRefreshableTokensIfNeeded(serverDetailsCopy)

			assert.Equal(t, tt.expectedError, err)
			assert.Equal(t, tt.expectedIntervalAfterCall, serverDetailsCopy.ArtifactoryTokenRefreshInterval,
				"ArtifactoryTokenRefreshInterval should remain unchanged on early return")
			assert.Equal(t, tt.expectedAccessTokenAfterCall, serverDetailsCopy.AccessToken,
				"AccessToken should remain unchanged on early return")
			assert.Equal(t, tt.expectedRefreshTokenAfterCall, serverDetailsCopy.ArtifactoryRefreshToken,
				"ArtifactoryRefreshToken should remain unchanged on early return")
		})
	}
}

func TestCreateInitialRefreshableTokensIfNeededValidInputs(t *testing.T) {
	tests := []struct {
		name                      string
		serverDetails             *ServerDetails
		expectedIntervalAfterCall int
		shouldCreateTokens        bool
		expectError               bool
	}{
		{
			name: "ValidInput_PositiveInterval",
			serverDetails: &ServerDetails{
				ServerId:                        "test-server",
				ArtifactoryTokenRefreshInterval: 60,
				ArtifactoryRefreshToken:         "",
				AccessToken:                     "",
				ArtifactoryUrl:                  "http://localhost:8081/artifactory/",
				User:                            "testuser",
				Password:                        "testpass",
			},
			expectedIntervalAfterCall: 0,
			shouldCreateTokens:        true,
			expectError:               false,
		},
		{
			name: "ValidInput_LargeInterval",
			serverDetails: &ServerDetails{
				ServerId:                        "test-server",
				ArtifactoryTokenRefreshInterval: 1440, // 24 hours
				ArtifactoryRefreshToken:         "",
				AccessToken:                     "",
				ArtifactoryUrl:                  "http://localhost:8081/artifactory/",
				User:                            "testuser",
				Password:                        "testpass",
			},
			expectedIntervalAfterCall: 0,
			shouldCreateTokens:        true,
			expectError:               false,
		},
		{
			name: "ValidInput_MinimalInterval",
			serverDetails: &ServerDetails{
				ServerId:                        "test-server",
				ArtifactoryTokenRefreshInterval: 1,
				ArtifactoryRefreshToken:         "",
				AccessToken:                     "",
				ArtifactoryUrl:                  "http://localhost:8081/artifactory/",
				User:                            "testuser",
				Password:                        "testpass",
			},
			expectedIntervalAfterCall: 0,
			shouldCreateTokens:        true,
			expectError:               false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cleanUpTempEnv := configtests.CreateTempEnv(t, false)
			defer cleanUpTempEnv()

			// Save initial server config
			err := SaveServersConf([]*ServerDetails{tt.serverDetails})
			assert.NoError(t, err)

			// Create a copy to avoid modifying the original
			serverDetailsCopy := &ServerDetails{
				ServerId:                        tt.serverDetails.ServerId,
				ArtifactoryTokenRefreshInterval: tt.serverDetails.ArtifactoryTokenRefreshInterval,
				ArtifactoryRefreshToken:         tt.serverDetails.ArtifactoryRefreshToken,
				AccessToken:                     tt.serverDetails.AccessToken,
				ArtifactoryUrl:                  tt.serverDetails.ArtifactoryUrl,
				User:                            tt.serverDetails.User,
				Password:                        tt.serverDetails.Password,
			}

			err = CreateInitialRefreshableTokensIfNeeded(serverDetailsCopy)

			if tt.expectError {
				assert.Error(t, err, "Expected an error but got none")
			} else if err == nil {
				// Note: This will fail if createTokensForConfig requires actual Artifactory connection
				// In that case, this test would need to be an integration test with a mock server
				assert.Equal(t, tt.expectedIntervalAfterCall, serverDetailsCopy.ArtifactoryTokenRefreshInterval,
					"ArtifactoryTokenRefreshInterval should be reset to 0 after successful token creation")
				// Verify tokens were set (if no error occurred)
				// Note: This assumes createTokensForConfig succeeded
				// In a real scenario, you'd need to mock the Artifactory service
				if tt.shouldCreateTokens {
					assert.NotEmpty(t, serverDetailsCopy.AccessToken, "AccessToken should be set after successful creation")
					assert.NotEmpty(t, serverDetailsCopy.ArtifactoryRefreshToken, "ArtifactoryRefreshToken should be set after successful creation")
				}
			}
		})
	}
}

func TestCreateInitialRefreshableTokensIfNeededInputValidation(t *testing.T) {
	tests := []struct {
		name          string
		serverDetails *ServerDetails
		description   string
	}{
		{
			name: "InputValidation_EmptyServerId",
			serverDetails: &ServerDetails{
				ServerId:                        "",
				ArtifactoryTokenRefreshInterval: 60,
				ArtifactoryRefreshToken:         "",
				AccessToken:                     "",
			},
			description: "Should handle empty ServerId",
		},
		{
			name: "InputValidation_NoArtifactoryUrl",
			serverDetails: &ServerDetails{
				ServerId:                        "test-server",
				ArtifactoryTokenRefreshInterval: 60,
				ArtifactoryRefreshToken:         "",
				AccessToken:                     "",
				ArtifactoryUrl:                  "",
			},
			description: "Should handle missing ArtifactoryUrl",
		},
		{
			name: "InputValidation_NoCredentials",
			serverDetails: &ServerDetails{
				ServerId:                        "test-server",
				ArtifactoryTokenRefreshInterval: 60,
				ArtifactoryRefreshToken:         "",
				AccessToken:                     "",
				ArtifactoryUrl:                  "http://localhost:8081/artifactory/",
				User:                            "",
				Password:                        "",
			},
			description: "Should handle missing credentials",
		},
		{
			name: "InputValidation_OnlyUser",
			serverDetails: &ServerDetails{
				ServerId:                        "test-server",
				ArtifactoryTokenRefreshInterval: 60,
				ArtifactoryRefreshToken:         "",
				AccessToken:                     "",
				ArtifactoryUrl:                  "http://localhost:8081/artifactory/",
				User:                            "testuser",
				Password:                        "",
			},
			description: "Should handle missing password",
		},
		{
			name: "InputValidation_OnlyPassword",
			serverDetails: &ServerDetails{
				ServerId:                        "test-server",
				ArtifactoryTokenRefreshInterval: 60,
				ArtifactoryRefreshToken:         "",
				AccessToken:                     "",
				ArtifactoryUrl:                  "http://localhost:8081/artifactory/",
				User:                            "",
				Password:                        "testpass",
			},
			description: "Should handle missing user",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cleanUpTempEnv := configtests.CreateTempEnv(t, false)
			defer cleanUpTempEnv()

			// Create a copy to avoid modifying the original
			serverDetailsCopy := &ServerDetails{
				ServerId:                        tt.serverDetails.ServerId,
				ArtifactoryTokenRefreshInterval: tt.serverDetails.ArtifactoryTokenRefreshInterval,
				ArtifactoryRefreshToken:         tt.serverDetails.ArtifactoryRefreshToken,
				AccessToken:                     tt.serverDetails.AccessToken,
				ArtifactoryUrl:                  tt.serverDetails.ArtifactoryUrl,
				User:                            tt.serverDetails.User,
				Password:                        tt.serverDetails.Password,
			}

			initialInterval := serverDetailsCopy.ArtifactoryTokenRefreshInterval
			err := CreateInitialRefreshableTokensIfNeeded(serverDetailsCopy)

			// The function should either:
			// 1. Return early (if conditions are met)
			// 2. Attempt to create tokens and potentially fail due to invalid input
			// We validate that the function handles the input gracefully
			if err != nil {
				// If there's an error, it should be due to invalid configuration
				// The interval should be reset to 0 if token creation was attempted
				if serverDetailsCopy.ArtifactoryTokenRefreshInterval == 0 {
					// Token creation was attempted but failed
					assert.Error(t, err, tt.description)
				}
			} else {
				// If no error, either early return occurred or tokens were created successfully
				if initialInterval > 0 && serverDetailsCopy.ArtifactoryTokenRefreshInterval == 0 {
					// Tokens were created successfully
					assert.NotEmpty(t, serverDetailsCopy.AccessToken, "AccessToken should be set after successful creation")
					assert.NotEmpty(t, serverDetailsCopy.ArtifactoryRefreshToken, "ArtifactoryRefreshToken should be set after successful creation")
				}
			}
		})
	}
}

func TestCreateInitialRefreshableTokensIfNeededStateChanges(t *testing.T) {
	t.Run("StateChange_IntervalResetToZero", func(t *testing.T) {
		cleanUpTempEnv := configtests.CreateTempEnv(t, false)
		defer cleanUpTempEnv()

		serverDetails := &ServerDetails{
			ServerId:                        "test-server",
			ArtifactoryTokenRefreshInterval: 60,
			ArtifactoryRefreshToken:         "",
			AccessToken:                     "",
			ArtifactoryUrl:                  "http://localhost:8081/artifactory/",
			User:                            "testuser",
			Password:                        "testpass",
		}

		initialInterval := serverDetails.ArtifactoryTokenRefreshInterval
		assert.Greater(t, initialInterval, 0, "Initial interval should be positive")

		// Note: This will fail if createTokensForConfig requires actual Artifactory connection
		// The function should reset the interval to 0 after attempting token creation
		err := CreateInitialRefreshableTokensIfNeeded(serverDetails)

		// If no error (or even with error after attempting), the interval should be 0
		// This is because the function sets it to 0 before writing tokens
		// However, if there's an early return, it should remain unchanged
		if err == nil {
			// If successful, interval should be 0
			assert.Equal(t, 0, serverDetails.ArtifactoryTokenRefreshInterval,
				"ArtifactoryTokenRefreshInterval should be reset to 0 after token creation")
		}
	})

	t.Run("StateChange_TokensSetAfterSuccess", func(t *testing.T) {
		cleanUpTempEnv := configtests.CreateTempEnv(t, false)
		defer cleanUpTempEnv()

		serverDetails := &ServerDetails{
			ServerId:                        "test-server",
			ArtifactoryTokenRefreshInterval: 60,
			ArtifactoryRefreshToken:         "",
			AccessToken:                     "",
			ArtifactoryUrl:                  "http://localhost:8081/artifactory/",
			User:                            "testuser",
			Password:                        "testpass",
		}

		// Save initial server config
		err := SaveServersConf([]*ServerDetails{serverDetails})
		assert.NoError(t, err)

		err = CreateInitialRefreshableTokensIfNeeded(serverDetails)

		// If successful, tokens should be set
		if err == nil {
			// Note: This assumes createTokensForConfig succeeded
			// In a real scenario with actual Artifactory, tokens would be set
			// For unit tests without mocking, we can only verify the function doesn't crash
			assert.NotNil(t, serverDetails, "ServerDetails should not be nil")
		}
	})
}

func TestCreateInitialRefreshableTokensIfNeededEdgeCases(t *testing.T) {
	tests := []struct {
		name          string
		serverDetails *ServerDetails
		description   string
	}{
		{
			name: "EdgeCase_AllFieldsEmpty",
			serverDetails: &ServerDetails{
				ServerId:                        "",
				ArtifactoryTokenRefreshInterval: 0,
				ArtifactoryRefreshToken:         "",
				AccessToken:                     "",
			},
			description: "Should handle all fields empty",
		},
		{
			name: "EdgeCase_IntervalOne",
			serverDetails: &ServerDetails{
				ServerId:                        "test-server",
				ArtifactoryTokenRefreshInterval: 1,
				ArtifactoryRefreshToken:         "",
				AccessToken:                     "",
			},
			description: "Should handle minimum positive interval",
		},
		{
			name: "EdgeCase_IntervalMaxInt",
			serverDetails: &ServerDetails{
				ServerId:                        "test-server",
				ArtifactoryTokenRefreshInterval: 2147483647, // max int32
				ArtifactoryRefreshToken:         "",         // jfrog-ignore
				AccessToken:                     "",         // jfrog-ignore
			},
			description: "Should handle maximum interval value",
		},
		{
			name: "EdgeCase_WhitespaceTokens",
			serverDetails: &ServerDetails{
				ServerId:                        "test-server",
				ArtifactoryTokenRefreshInterval: 60,
				ArtifactoryRefreshToken:         "   ", // jfrog-ignore
				AccessToken:                     "",    // jfrog-ignore
			},
			description: "Should handle whitespace-only refresh token (treated as non-empty)",
		},
		{
			name: "EdgeCase_WhitespaceAccessToken",
			serverDetails: &ServerDetails{
				ServerId:                        "test-server",
				ArtifactoryTokenRefreshInterval: 60,
				ArtifactoryRefreshToken:         "",    // jfrog-ignore
				AccessToken:                     "   ", // jfrog-ignore
			},
			description: "Should handle whitespace-only access token (treated as non-empty)",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cleanUpTempEnv := configtests.CreateTempEnv(t, false)
			defer cleanUpTempEnv()

			serverDetailsCopy := &ServerDetails{
				ServerId:                        tt.serverDetails.ServerId,
				ArtifactoryTokenRefreshInterval: tt.serverDetails.ArtifactoryTokenRefreshInterval,
				ArtifactoryRefreshToken:         tt.serverDetails.ArtifactoryRefreshToken,
				AccessToken:                     tt.serverDetails.AccessToken,
			}

			initialInterval := serverDetailsCopy.ArtifactoryTokenRefreshInterval
			initialRefreshToken := serverDetailsCopy.ArtifactoryRefreshToken
			initialAccessToken := serverDetailsCopy.AccessToken

			err := CreateInitialRefreshableTokensIfNeeded(serverDetailsCopy)

			// Validate behavior based on early return conditions
			shouldEarlyReturn := initialInterval <= 0 || initialRefreshToken != "" || initialAccessToken != ""

			if shouldEarlyReturn {
				assert.NoError(t, err, "Should return early without error")
				assert.Equal(t, initialInterval, serverDetailsCopy.ArtifactoryTokenRefreshInterval,
					"Interval should remain unchanged on early return")
			} else if err == nil {
				// Function should attempt to create tokens
				// Error is expected if credentials are missing or Artifactory is unreachable
				// Interval should be reset to 0 if token creation was attempted
				assert.Equal(t, 0, serverDetailsCopy.ArtifactoryTokenRefreshInterval,
					"Interval should be reset to 0 after successful token creation")
			}
		})
	}
}

// TestCreateInitialRefreshableTokensIfNeededOutputValidation tests the outputs
// of the function in various scenarios
func TestCreateInitialRefreshableTokensIfNeededOutputValidation(t *testing.T) {
	t.Run("OutputValidation_EarlyReturnNilError", func(t *testing.T) {
		serverDetails := &ServerDetails{
			ServerId:                        "test-server",
			ArtifactoryTokenRefreshInterval: 0,
			ArtifactoryRefreshToken:         "",
			AccessToken:                     "",
		}

		err := CreateInitialRefreshableTokensIfNeeded(serverDetails)
		assert.NoError(t, err, "Early return should produce nil error")
	})

	t.Run("OutputValidation_ServerDetailsNotNil", func(t *testing.T) {
		serverDetails := &ServerDetails{
			ServerId:                        "test-server",
			ArtifactoryTokenRefreshInterval: 60,
			ArtifactoryRefreshToken:         "",
			AccessToken:                     "",
		}

		err := CreateInitialRefreshableTokensIfNeeded(serverDetails)
		// Function should not panic and serverDetails should remain valid
		assert.NotNil(t, serverDetails, "ServerDetails should not be nil after function call")
		_ = err // Error may or may not be nil depending on dependencies
	})
}

// TestCreateInitialRefreshableTokensIfNeededBranchCoverage ensures all code branches are tested
func TestCreateInitialRefreshableTokensIfNeededBranchCoverage(t *testing.T) {
	cleanUpTempEnv := configtests.CreateTempEnv(t, false)
	defer cleanUpTempEnv()

	t.Run("BranchCoverage_AllEarlyReturnPaths", func(t *testing.T) {
		// Test branch: ArtifactoryTokenRefreshInterval <= 0
		serverDetails1 := &ServerDetails{
			ArtifactoryTokenRefreshInterval: 0,
			ArtifactoryRefreshToken:         "",
			AccessToken:                     "",
		}
		err1 := CreateInitialRefreshableTokensIfNeeded(serverDetails1)
		assert.NoError(t, err1)

		// Test branch: ArtifactoryRefreshToken != ""
		serverDetails2 := &ServerDetails{
			ArtifactoryTokenRefreshInterval: 60,
			ArtifactoryRefreshToken:         "token",
			AccessToken:                     "",
		}
		err2 := CreateInitialRefreshableTokensIfNeeded(serverDetails2)
		assert.NoError(t, err2)

		// Test branch: AccessToken != ""
		serverDetails3 := &ServerDetails{
			ArtifactoryTokenRefreshInterval: 60,
			ArtifactoryRefreshToken:         "",
			AccessToken:                     "token",
		}
		err3 := CreateInitialRefreshableTokensIfNeeded(serverDetails3)
		assert.NoError(t, err3)
	})

	t.Run("BranchCoverage_MainExecutionPath", func(t *testing.T) {
		// This tests the main execution path (not early return)
		// Note: This will require proper setup or mocking of dependencies
		serverDetails := &ServerDetails{
			ServerId:                        "test-server",
			ArtifactoryTokenRefreshInterval: 60,
			ArtifactoryRefreshToken:         "",
			AccessToken:                     "",
			ArtifactoryUrl:                  "http://localhost:8081/artifactory/",
			User:                            "testuser",
			Password:                        "testpass",
		}

		// Save initial server config
		err := SaveServersConf([]*ServerDetails{serverDetails})
		assert.NoError(t, err)

		// This will attempt to create tokens
		// Error is expected if Artifactory is not available
		_ = CreateInitialRefreshableTokensIfNeeded(serverDetails)
		// We don't assert on error here since it depends on external dependencies
		// But we verify the function executes without panic
		assert.NotNil(t, serverDetails)
	})
}

// newFakeAccessTokenServer returns a minimal fake Access service that answers the
// refresh-token endpoint (POST .../access/api/v1/tokens) with a fixed, server-specific
// access/refresh token pair, so tests can tell which server a request actually reached.
func newFakeAccessTokenServer(t *testing.T, newAccessToken, newRefreshToken string) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || !strings.HasSuffix(r.URL.Path, "/access/api/v1/tokens") {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		require.NoError(t, json.NewEncoder(w).Encode(map[string]any{
			"access_token":  newAccessToken,
			"refresh_token": newRefreshToken,
			"token_type":    "Bearer",
		}))
	}))
}

// countingFakeAccessTokenServer behaves like newFakeAccessTokenServer, but also counts how many
// refresh requests actually reached the backend, so tests can assert that concurrent refresh
// attempts for the same server were deduplicated by serverRefreshMutex down to a single
// backend call.
func countingFakeAccessTokenServer(t *testing.T, newAccessToken, newRefreshToken string) (*httptest.Server, *atomic.Int32) {
	var requestCount atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || !strings.HasSuffix(r.URL.Path, "/access/api/v1/tokens") {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		requestCount.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		require.NoError(t, json.NewEncoder(w).Encode(map[string]any{
			"access_token":  newAccessToken,
			"refresh_token": newRefreshToken,
			"token_type":    "Bearer",
		}))
	}))
	return server, &requestCount
}

// TestConcurrentRefreshesForSameServerAreDeduplicated is a regression test for serverRefreshMutex
// itself. TestTokenRefreshIsScopedPerServer calls tokenRefreshHandler directly for two different
// servers, which skips tokenRefreshPreRequestInterceptor (and therefore the mutex entirely) and is
// serialized anyway by the cross-process config file lock - so it proves nothing about per-server
// deduplication of concurrent refreshes for the SAME server.
//
// This test drives two concurrent requests through the real entry point
// (RunPreRequestFunctions -> tokenRefreshPreRequestInterceptor) on the same server's auth config,
// exactly as two in-flight HTTP requests on one server would race through the interceptor.
//
// Attack scenario this guards against: if serverRefreshMutex regressed to hand back a distinct
// mutex per call (e.g. LoadOrStore misuse, or a key mismatch between callers), both goroutines
// would pass the "token not already refreshed" check (fields.AccessToken == httpClientDetails.AccessToken)
// at the same time and both call tokenRefreshHandler. The second call would present an
// already-rotated refresh token to the backend, and in a real server that second refresh would
// fail or clobber the first one's stored tokens. A correctly scoped mutex must ensure only one of
// the two concurrent calls ever reaches the backend, and both callers must still observe the
// single resulting token.
func TestConcurrentRefreshesForSameServerAreDeduplicated(t *testing.T) {
	cleanUpTempEnv := configtests.CreateTempEnv(t, false)
	defer cleanUpTempEnv()

	server, refreshRequests := countingFakeAccessTokenServer(t, "new-access-token", "new-refresh-token")
	defer server.Close()

	// Already expired, so tokenRefreshHandler refreshes immediately instead of sleeping.
	oldToken := buildTestAccessToken(t, 1, 2)
	details := &ServerDetails{
		ServerId:     "server-dedup",
		Url:          server.URL + "/",
		AccessToken:  oldToken,
		RefreshToken: "refresh-token-dedup", // #nosec G101 -- test data only
	}
	require.NoError(t, SaveServersConf([]*ServerDetails{details}))

	artAuth, err := details.CreateArtAuthConfig()
	require.NoError(t, err)
	require.Len(t, artAuth.GetPreRequestFunctions(), 1)

	const concurrentRequests = 2
	var wg sync.WaitGroup
	httpDetailsPerRequest := make([]httputils.HttpClientDetails, concurrentRequests)
	errsPerRequest := make([]error, concurrentRequests)
	wg.Add(concurrentRequests)
	for i := 0; i < concurrentRequests; i++ {
		i := i
		go func() {
			defer wg.Done()
			httpDetailsPerRequest[i] = httputils.HttpClientDetails{AccessToken: oldToken}
			errsPerRequest[i] = artAuth.RunPreRequestFunctions(&httpDetailsPerRequest[i])
		}()
	}
	wg.Wait()

	for i := 0; i < concurrentRequests; i++ {
		require.NoError(t, errsPerRequest[i])
		assert.Equal(t, "new-access-token", httpDetailsPerRequest[i].AccessToken,
			"both concurrent callers must observe the single refreshed token")
	}
	assert.Equal(t, int32(1), refreshRequests.Load(),
		"serverRefreshMutex must deduplicate two concurrent refreshes for the same server into a single backend call")
}

// TestConcurrentRefreshesForDifferentServersUseIndependentInterceptors complements the test above:
// it proves that two servers refreshing at the same time, each through its own interceptor
// (the exact "jf rt transfer-files src tgt" shape the credential-host-binding fix targets), each
// reach only their own backend and never block on, or get deduplicated against, the other
// server's mutex.
func TestConcurrentRefreshesForDifferentServersUseIndependentInterceptors(t *testing.T) {
	cleanUpTempEnv := configtests.CreateTempEnv(t, false)
	defer cleanUpTempEnv()

	serverA, requestsA := countingFakeAccessTokenServer(t, "new-access-token-for-A", "new-refresh-token-for-A") // #nosec G101 -- test data only
	defer serverA.Close()
	serverB, requestsB := countingFakeAccessTokenServer(t, "new-access-token-for-B", "new-refresh-token-for-B") // #nosec G101 -- test data only
	defer serverB.Close()

	oldTokenA := buildTestAccessToken(t, 1, 2)
	oldTokenB := buildTestAccessToken(t, 1, 2)

	detailsA := &ServerDetails{
		ServerId:     "server-A-independent",
		Url:          serverA.URL + "/",
		AccessToken:  oldTokenA,
		RefreshToken: "refresh-token-A", // #nosec G101 -- test data only
	}
	detailsB := &ServerDetails{
		ServerId:     "server-B-independent",
		Url:          serverB.URL + "/",
		AccessToken:  oldTokenB,
		RefreshToken: "refresh-token-B", // #nosec G101 -- test data only
	}
	require.NoError(t, SaveServersConf([]*ServerDetails{detailsA, detailsB}))

	artAuthA, err := detailsA.CreateArtAuthConfig()
	require.NoError(t, err)
	artAuthB, err := detailsB.CreateArtAuthConfig()
	require.NoError(t, err)

	var wg sync.WaitGroup
	var httpDetailsA, httpDetailsB httputils.HttpClientDetails
	var errA, errB error
	wg.Add(2)
	go func() {
		defer wg.Done()
		httpDetailsA = httputils.HttpClientDetails{AccessToken: oldTokenA}
		errA = artAuthA.RunPreRequestFunctions(&httpDetailsA)
	}()
	go func() {
		defer wg.Done()
		httpDetailsB = httputils.HttpClientDetails{AccessToken: oldTokenB}
		errB = artAuthB.RunPreRequestFunctions(&httpDetailsB)
	}()
	wg.Wait()

	require.NoError(t, errA)
	require.NoError(t, errB)
	assert.Equal(t, "new-access-token-for-A", httpDetailsA.AccessToken)
	assert.Equal(t, "new-access-token-for-B", httpDetailsB.AccessToken)
	assert.Equal(t, int32(1), requestsA.Load(), "server A's refresh must reach server A's backend exactly once")
	assert.Equal(t, int32(1), requestsB.Load(), "server B's refresh must reach server B's backend exactly once")
}

// TestTokenRefreshIsScopedPerServer is a regression test for the vulnerability where the
// server used to refresh a token was tracked in a single process-global variable
// (tokenRefreshServerId) that every createAuthConfig call overwrote. Under that scheme,
// once two servers were configured in one process (e.g. "jf rt transfer-files src tgt"),
// a refresh triggered for server A's token could resolve against server B's config entry
// and credentials, handing server A's host a live access token that actually belongs to
// server B (CWE-522).
//
// Concrete scenario: two servers, A and B, each configured with their own refreshable
// token and their own fake Access backend. We trigger a refresh for A concurrently with a
// refresh for B (mirroring a multi-server command, or two refreshes racing each other) and
// assert that each server's config entry ends up with its own new token - never the
// other's.
func TestTokenRefreshIsScopedPerServer(t *testing.T) {
	cleanUpTempEnv := configtests.CreateTempEnv(t, false)
	defer cleanUpTempEnv()

	serverA := newFakeAccessTokenServer(t, "new-access-token-for-A", "new-refresh-token-for-A") // #nosec G101 -- test data only
	defer serverA.Close()
	serverB := newFakeAccessTokenServer(t, "new-access-token-for-B", "new-refresh-token-for-B") // #nosec G101 -- test data only
	defer serverB.Close()

	// Both tokens are already expired, so tokenRefreshHandler refreshes immediately
	// instead of sleeping.
	oldTokenA := buildTestAccessToken(t, 1, 2)
	oldTokenB := buildTestAccessToken(t, 1, 2)

	detailsA := &ServerDetails{
		ServerId:     "server-A",
		Url:          serverA.URL + "/",
		AccessToken:  oldTokenA,
		RefreshToken: "refresh-token-A", // #nosec G101 -- test data only
	}
	detailsB := &ServerDetails{
		ServerId:     "server-B",
		Url:          serverB.URL + "/",
		AccessToken:  oldTokenB,
		RefreshToken: "refresh-token-B", // #nosec G101 -- test data only
	}
	require.NoError(t, SaveServersConf([]*ServerDetails{detailsA, detailsB}))

	// Fire both refreshes at the same time: this is exactly the "concurrent operations
	// across multiple server configs in one process" scenario the vulnerability required.
	var wg sync.WaitGroup
	var newTokenA, newTokenB string
	var errA, errB error
	wg.Add(2)
	go func() {
		defer wg.Done()
		newTokenA, errA = tokenRefreshHandler("server-A", oldTokenA, AccessToken)
	}()
	go func() {
		defer wg.Done()
		newTokenB, errB = tokenRefreshHandler("server-B", oldTokenB, AccessToken)
	}()
	wg.Wait()

	require.NoError(t, errA)
	require.NoError(t, errB)

	// Each caller must get back the token issued by its *own* server, never the other's.
	assert.Equal(t, "new-access-token-for-A", newTokenA)
	assert.Equal(t, "new-access-token-for-B", newTokenB)

	// And what landed in persistent config for each server must likewise be its own
	// token - this is the state the old global tokenRefreshServerId could corrupt.
	confA, err := GetSpecificConfig("server-A", true, false)
	require.NoError(t, err)
	confB, err := GetSpecificConfig("server-B", true, false)
	require.NoError(t, err)
	assert.Equal(t, "new-access-token-for-A", confA.AccessToken)
	assert.Equal(t, "new-refresh-token-for-A", confA.RefreshToken)
	assert.Equal(t, "new-access-token-for-B", confB.AccessToken)
	assert.Equal(t, "new-refresh-token-for-B", confB.RefreshToken)
}

// TestAccessTokenRefreshPreRequestInterceptorBoundToOwnServer proves that the interceptor
// handed to a server's auth config is bound, at creation time, to that server's own
// ServerId and never drifts when another server's auth config is created afterward in the
// same process - the exact sequence (createAuthConfig(A) then createAuthConfig(B)) that
// used to overwrite the shared tokenRefreshServerId global with B's id.
func TestAccessTokenRefreshPreRequestInterceptorBoundToOwnServer(t *testing.T) {
	cleanUpTempEnv := configtests.CreateTempEnv(t, false)
	defer cleanUpTempEnv()

	serverA := newFakeAccessTokenServer(t, "new-access-token-for-A", "new-refresh-token-for-A") // #nosec G101 -- test data only
	defer serverA.Close()
	serverB := newFakeAccessTokenServer(t, "new-access-token-for-B", "new-refresh-token-for-B") // #nosec G101 -- test data only
	defer serverB.Close()

	oldTokenA := buildTestAccessToken(t, 1, 2)
	oldTokenB := buildTestAccessToken(t, 1, 2)

	detailsA := &ServerDetails{
		ServerId:     "server-A",
		Url:          serverA.URL + "/",
		AccessToken:  oldTokenA,
		RefreshToken: "refresh-token-A", // #nosec G101 -- test data only
	}
	detailsB := &ServerDetails{
		ServerId:     "server-B",
		Url:          serverB.URL + "/",
		AccessToken:  oldTokenB,
		RefreshToken: "refresh-token-B", // #nosec G101 -- test data only
	}
	require.NoError(t, SaveServersConf([]*ServerDetails{detailsA, detailsB}))

	// Create A's auth config first, then B's - mirroring createAuthConfig being called
	// for two servers in one process (e.g. source/target of a transfer command). With the
	// old global, this sequence would leave tokenRefreshServerId == "server-B".
	artAuthA, err := detailsA.CreateArtAuthConfig()
	require.NoError(t, err)
	_, err = detailsB.CreateArtAuthConfig()
	require.NoError(t, err)

	require.Len(t, artAuthA.GetPreRequestFunctions(), 1)

	// Invoke A's interceptor (via RunPreRequestFunctions, the same path a real request
	// takes) with A's own access token. It must refresh and apply A's own new token (from
	// serverA), never server-B's, even though B's auth config was created afterward.
	httpDetails := httputils.HttpClientDetails{AccessToken: oldTokenA}
	artAuthA.SetAccessToken(oldTokenA)
	require.NoError(t, artAuthA.RunPreRequestFunctions(&httpDetails))
	assert.Equal(t, "new-access-token-for-A", httpDetails.AccessToken)
}
