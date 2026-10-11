package tests

import (
	"testing"

	"cs2-maintainer/core"
)

const sampleAppInfoOutput = `Redirecting stderr to 'C:\Steam\logs\stderr.txt'
[  0%] Checking for available updates...
Loading Steam API...OK
Connecting anonymously to Steam Public...OK
AppID : 730, change number : 29912345/0, last change : Thu Oct  8 18:01:12 2026
"730"
{
	"common"
	{
		"name"		"Counter-Strike 2"
		"type"		"Game"
	}
	"config"
	{
		"launch"
		{
			"0"
			{
				"executable"		"game\bin\win64\cs2.exe"
			}
		}
	}
	"depots"
	{
		"branches"
		{
			"public"
			{
				"buildid"		"20412345"
				"timeupdated"		"1791480000"
			}
			"animgraph_2_beta"
			{
				"buildid"		"20400001"
				"pwdrequired"		"1"
			}
		}
	}
}
Unloading Steam API...OK
`

func TestExtractAppInfo(t *testing.T) {
	info, err := core.ExtractAppInfo(sampleAppInfoOutput, 730)
	if err != nil {
		t.Fatal(err)
	}
	if id, _ := info.Get("depots", "branches", "public", "buildid"); id != "20412345" {
		t.Errorf("public buildid = %q", id)
	}
	if id, _ := info.Get("depots", "branches", "animgraph_2_beta", "buildid"); id != "20400001" {
		t.Errorf("beta buildid = %q", id)
	}
	if exe, _ := info.Get("config", "launch", "0", "executable"); exe != `game\bin\win64\cs2.exe` {
		t.Errorf("backslashes mangled: %q", exe)
	}
}

func TestExtractAppInfoMissing(t *testing.T) {
	if _, err := core.ExtractAppInfo("AppID : 730, change number : 1\nLoading Steam API...OK\n", 730); err == nil {
		t.Error("expected an error when the block is missing")
	}
}

func TestParseManifest(t *testing.T) {
	acf := `"AppState"
{
	"appid"		"730"
	"StateFlags"		"4"
	"buildid"		"20398765"
	"InstalledDepots"
	{
		"2347771" { "manifest" "123" "size" "456" }
	}
	// a comment
	"UserConfig" { "language" "english" }
}`
	root, err := core.ParseVDF(acf)
	if err != nil {
		t.Fatal(err)
	}
	if id, ok := root.Get("AppState", "BuildID"); !ok || id != "20398765" {
		t.Errorf("buildid = %q, %v", id, ok)
	}
}

func TestParseVDFErrors(t *testing.T) {
	for _, bad := range []string{`"a" {`, `"a" "b" }`, `"unterminated`, `{ "a" "b" }`} {
		if _, err := core.ParseVDF(bad); err == nil {
			t.Errorf("core.ParseVDF(%q) succeeded", bad)
		}
	}
}

func TestBuildNewer(t *testing.T) {
	cases := []struct {
		a, b string
		want bool
	}{
		{"200", "100", true},
		{"100", "200", false},
		{"100", "100", false},
		{"100", "", true},
		{"", "100", false},
		{"9", "10", false}, // numeric, not lexical
	}
	for _, c := range cases {
		if got := core.BuildNewer(c.a, c.b); got != c.want {
			t.Errorf("core.BuildNewer(%q, %q) = %v", c.a, c.b, got)
		}
	}
}
