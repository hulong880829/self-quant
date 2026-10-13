package clickhouse

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

func TestCryptoBBOSchemaMatchesRecorder(t *testing.T) {
	sqlBytes, err := os.ReadFile("migrations/000_crypto_bbo.sql")
	if err != nil {
		t.Fatal(err)
	}
	recorder, err := os.ReadFile("../../../core/mds/src/record/clickhouse_bbo_recorder.cpp")
	if err != nil {
		t.Fatal(err)
	}
	got := schemaShape(string(sqlBytes))
	want := schemaShape(recorderDDL(string(recorder)))
	if got != want {
		t.Fatalf("crypto_bbo schema drifted\nmigration: %s\nrecorder:   %s", got, want)
	}
}

func recorderDDL(source string) string {
	marker := "const auto ddl ="
	start := strings.Index(source, marker)
	if start < 0 {
		return ""
	}
	rest := source[start+len(marker):]
	end := strings.Index(rest, ";")
	if end < 0 {
		return ""
	}
	statement := rest[:end]
	var b strings.Builder
	for {
		open := strings.Index(statement, `"`)
		if open < 0 {
			break
		}
		statement = statement[open+1:]
		close := strings.Index(statement, `"`)
		if close < 0 {
			break
		}
		b.WriteString(statement[:close])
		statement = statement[close+1:]
	}
	return b.String()
}

func schemaShape(statement string) string {
	collapsed := strings.Join(strings.Fields(statement), " ")
	collapsed = regexp.MustCompile(`ENGINE\s*=\s*`).ReplaceAllString(collapsed, "ENGINE=")
	columns := columnList(collapsed)
	engine := regexp.MustCompile(`ENGINE=([A-Za-z]+)`).FindStringSubmatch(collapsed)
	partition := regexp.MustCompile(`PARTITION BY (.+?) ORDER BY`).FindStringSubmatch(collapsed)
	order := regexp.MustCompile(`ORDER BY (\([^)]+\))`).FindStringSubmatch(collapsed)
	if columns == "" || len(engine) != 2 || len(partition) != 2 || len(order) != 2 {
		return "unparsed:" + collapsed
	}
	return strings.Join([]string{
		strings.Join(strings.Fields(columns), " "),
		engine[1],
		strings.TrimSpace(partition[1]),
		strings.TrimSpace(order[1]),
	}, "|")
}

func columnList(statement string) string {
	engine := strings.Index(statement, "ENGINE=")
	if engine < 0 {
		return ""
	}
	closeParen := strings.LastIndex(statement[:engine], ")")
	if closeParen < 0 {
		return ""
	}
	depth := 0
	for i := closeParen; i >= 0; i-- {
		switch statement[i] {
		case ')':
			depth++
		case '(':
			depth--
			if depth == 0 {
				return statement[i+1 : closeParen]
			}
		}
	}
	return ""
}
