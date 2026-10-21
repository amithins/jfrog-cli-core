package utils

import (
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"time"
	"unicode"

	"github.com/gocarina/gocsv"
	ioutils "github.com/jfrog/gofrog/io"
	logutils "github.com/jfrog/jfrog-cli-core/v2/utils/log"
	"github.com/jfrog/jfrog-client-go/http/jfroghttpclient"
	"github.com/jfrog/jfrog-client-go/utils/errorutils"
	"github.com/jfrog/jfrog-client-go/utils/io/httputils"
)

type ServerType string

const (
	Source                ServerType = "source"
	Target                ServerType = "target"
	PluginsExecuteRestApi            = "api/plugins/execute/"
)

func GetTransferPluginVersion(client *jfroghttpclient.JfrogHttpClient, url, pluginName string, serverType ServerType, rtDetails *httputils.HttpClientDetails) (string, error) {
	versionResp, versionBody, _, err := client.SendGet(url, false, rtDetails)
	if err != nil {
		return "", err
	}
	if versionResp.StatusCode == http.StatusOK {
		verRes := &VersionResponse{}
		err = json.Unmarshal(versionBody, verRes)
		if err != nil {
			return "", errorutils.CheckError(err)
		}
		return verRes.Version, nil
	}

	messageFormat := fmt.Sprintf("Response from Artifactory: %s.\n%s\n", versionResp.Status, versionBody)
	if versionResp.StatusCode == http.StatusNotFound {
		return "", errorutils.CheckErrorf("%sIt looks like the %s plugin is not installed on the %s server.", messageFormat, pluginName, serverType)
	} else {
		// 403 if the user is not admin, 500+ if there is a server error
		return "", errors.New(messageFormat)
	}
}

type VersionResponse struct {
	Version string `json:"version,omitempty"`
}

func CreateCSVFile(filePrefix string, items interface{}, timeStarted time.Time) (csvPath string, err error) {
	// Create CSV file
	summaryCsv, err := logutils.CreateCustomLogFile(fmt.Sprintf("%s-%s.csv", filePrefix, timeStarted.Format(logutils.DefaultLogTimeLayout)))
	if err != nil {
		return
	}
	csvPath = summaryCsv.Name()
	defer ioutils.Close(summaryCsv, &err)
	// Marshal JSON typed items array to CSV file
	err = errorutils.CheckError(gocsv.MarshalCSV(items, &sanitizingCsvWriter{Writer: csv.NewWriter(summaryCsv)}))
	return
}

// sanitizingCsvWriter neutralizes cells that spreadsheet applications would evaluate as formulas.
type sanitizingCsvWriter struct {
	*csv.Writer
}

func (w *sanitizingCsvWriter) Write(row []string) error {
	sanitized := make([]string, len(row))
	for i, cell := range row {
		sanitized[i] = sanitizeCsvCell(cell)
	}
	return w.Writer.Write(sanitized)
}

// csvCanonicalDecimal matches plain decimal numbers (optionally signed, with fraction and exponent).
// Anything else that starts with a sign, such as -Inf or -0x1p-2, is not treated as a number.
var csvCanonicalDecimal = regexp.MustCompile(`^[+-]?\d+(?:\.\d+)?(?:[eE][+-]?\d+)?$`)

// sanitizeCsvCell prefixes a single quote to cells that a spreadsheet could evaluate as a formula.
// Cells that are canonical decimal numbers are left untouched, so signed values such as -1 stay numeric.
// Leading whitespace, control and zero-width characters are skipped when looking for the trigger character.
// Spreadsheets in some locales split cells on ';' and tab, so every segment is checked, not only the first.
func sanitizeCsvCell(cell string) string {
	if cell == "" || csvCanonicalDecimal.MatchString(cell) {
		return cell
	}
	atSegmentStart := true
	for _, r := range cell {
		if atSegmentStart {
			if isCsvFormulaTrigger(r) {
				return "'" + cell
			}
			if isCsvIgnorablePrefix(r) {
				continue
			}
		}
		atSegmentStart = r == ';' || r == '\t'
	}
	return cell
}

func isCsvFormulaTrigger(r rune) bool {
	switch r {
	case '=', '+', '-', '@', '\t', '\r', '\n',
		'\uFF1D', '\uFF0B', '\uFF0D', '\uFF20', '\uFE62', '\uFE63', '\uFE66', '\uFE6B':
		return true
	}
	return false
}

// isCsvIgnorablePrefix reports characters that may precede a formula trigger without making the cell plain text.
func isCsvIgnorablePrefix(r rune) bool {
	switch r {
	case '\u200B', '\u200C', '\u200D', '\u2060', '\uFEFF':
		return true
	}
	return unicode.IsSpace(r) || unicode.IsControl(r)
}
