// SPDX-License-Identifier: AGPL-3.0-only

package sieve

import (
	"fmt"
	"reflect"
	"strings"
	"testing"
)

// A script in the shape provider webmails (Roundcube's managesieve plugin)
// generate: a require list, a bracketed comment header, rules, and a vacation
// with a multi-line body.
const webmailScript = `require ["fileinto","vacation","variables","date","relational"];
/* rule:[lists] */
if anyof (header :contains "list-id" "PRIVATE-MARKER-1", address :is "from" "PRIVATE-MARKER-2@example.com")
{
	fileinto "INBOX.PRIVATE-MARKER-3";
	stop;
}
elsif header :contains "subject" "vacation ; stop" { redirect "PRIVATE-MARKER-4@example.com"; }
else { keep; }
# rule:[away] vacation fileinto
if allof (currentdate :zone "+0000" :value "ge" "date" "2026-01-01")
{
	vacation :days 1 :addresses ["PRIVATE-MARKER-5@example.com"] :subject "PRIVATE-MARKER-6" text:
Away; back soon. vacation fileinto "PRIVATE-MARKER-7"
..leading dot
.
;
}
include :personal "PRIVATE-MARKER-8";
`

func TestSummarizeWebmailScript(t *testing.T) {
	s := Summarize(webmailScript)
	if !s.Complete {
		t.Fatalf("did not parse completely: stopped at %d (%s)", s.StoppedAt, s.Problem)
	}
	if want := []string{"date", "fileinto", "relational", "vacation", "variables"}; !reflect.DeepEqual(s.Require, want) {
		t.Errorf("require = %v", s.Require)
	}
	wantCmds := map[string]int{
		"require": 1, "if": 2, "elsif": 1, "else": 1, "fileinto": 1, "stop": 1,
		"redirect": 1, "keep": 1, "vacation": 1, "include": 1,
	}
	if !reflect.DeepEqual(s.Commands, wantCmds) {
		t.Errorf("commands = %v\nwant       %v", s.Commands, wantCmds)
	}
	wantTests := map[string]int{"anyof": 1, "header": 2, "address": 1, "allof": 1, "currentdate": 1}
	if !reflect.DeepEqual(s.Tests, wantTests) {
		t.Errorf("tests = %v\nwant    %v", s.Tests, wantTests)
	}
	if !s.Vacation || !reflect.DeepEqual(s.VacationTags, []string{":addresses", ":days", ":subject"}) {
		t.Errorf("vacation = %v tags %v", s.Vacation, s.VacationTags)
	}
	if s.Includes != 1 {
		t.Errorf("includes = %d", s.Includes)
	}
}

// TestSummaryCarriesNoScriptValues: no string from the script — addresses,
// folder names, the vacation text, an include target — survives into the
// Summary, which is what makes it printable when the body is not.
func TestSummaryCarriesNoScriptValues(t *testing.T) {
	dump := fmt.Sprintf("%+v %v %v", Summarize(webmailScript), SortedCounts(Summarize(webmailScript).Commands),
		SortedCounts(Summarize(webmailScript).Tests))
	if strings.Contains(dump, "PRIVATE-MARKER") || strings.Contains(dump, "example.com") {
		t.Fatalf("a script value leaked into the summary: %s", dump)
	}
}

// TestWordsInsideStringsAndCommentsAreNotCommands: "vacation" appearing only in
// a comment, a quoted string or a text: block is not a vacation action.
func TestWordsInsideStringsAndCommentsAreNotCommands(t *testing.T) {
	src := "# vacation\r\n/* vacation; */\r\nif header :is \"x\" \"vacation\" { keep; }\r\n" +
		"set \"t\" text:\r\nvacation :days 1 \"x\";\r\n.\r\n;\r\n"
	s := Summarize(src)
	if !s.Complete || s.Vacation || s.Commands["vacation"] != 0 || s.Commands["set"] != 1 {
		t.Fatalf("summary = %+v", s)
	}
}

// TestRealVacationIsSeen is the positive control for the test above.
func TestRealVacationIsSeen(t *testing.T) {
	s := Summarize("require \"vacation\";\nvacation :seconds 3600 \"back soon\";\n")
	if !s.Complete || !s.Vacation || !reflect.DeepEqual(s.VacationTags, []string{":seconds"}) ||
		!reflect.DeepEqual(s.Require, []string{"vacation"}) {
		t.Fatalf("summary = %+v", s)
	}
}

// TestIncompleteParseReportsItsBound: an unterminated construct stops the
// lexer, and the summary says where — its counts are a lower bound.
func TestIncompleteParseReportsItsBound(t *testing.T) {
	src := "keep;\nfileinto \"unterminated;\nvacation x;\n"
	s := Summarize(src)
	if s.Complete || s.StoppedAt != strings.Index(src, "\"unterminated") || s.Problem == "" {
		t.Fatalf("summary = %+v", s)
	}
	if s.Commands["keep"] != 1 || s.Vacation {
		t.Fatalf("counts before the stop are wrong: %+v", s)
	}
	for _, bad := range []string{"text:\nno terminator\n", "/* open", "keep; \x01", ": ;"} {
		if Summarize(bad).Complete {
			t.Errorf("%q parsed as complete", bad)
		}
	}
}

func TestEmptyScript(t *testing.T) {
	s := Summarize("")
	if !s.Complete || s.Bytes != 0 || s.Lines != 0 || len(s.Commands) != 0 {
		t.Fatalf("%+v", s)
	}
}

func TestSortedCounts(t *testing.T) {
	got := SortedCounts(map[string]int{"b": 1, "a": 1, "if": 3})
	if !reflect.DeepEqual(got, []string{"if×3", "a×1", "b×1"}) {
		t.Fatalf("%v", got)
	}
}
