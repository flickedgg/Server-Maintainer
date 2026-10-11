package tests

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"cs2-maintainer/core"
)

// stockGameInfo is the FileSystem part of CS2's gameinfo.gi as an update ships it.
const stockGameInfo = `"GameInfo"
{
	game 		"Counter-Strike 2"
	title 		"Counter-Strike 2"
	hidden_maps
	{
		"test_speakers"			1
	}

	FileSystem
	{
		//
		// The code that loads this file automatically does a few things here:
		// SearchPaths are mounted in order.
		//
		SearchPaths
		{
			Game_LowViolence	csgo_lv // Perfect World content override

			Game	csgo
			Game	csgo_imported
			Game	csgo_core
			Game	core

			Mod	csgo
			Mod	csgo_imported
			Mod	csgo_core

			AddonRoot_LowViolence	csgo_addons_lv // Perfect World content override

			AddonRoot	csgo_addons

			LayeredGameRoot		"../game_otherplatforms/etc" [$MOBILE || $ETC]
		}
	}
	Engine2
	{
		HasModAppSystems 1
	}
}
`

func TestAddGameInfoSearchPathsAfterLowViolence(t *testing.T) {
	out, added, err := core.AddGameInfoSearchPaths(stockGameInfo, []string{"csgo/addons/metamod"})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(added, []string{"csgo/addons/metamod"}) {
		t.Errorf("added = %v", added)
	}
	want := "\t\t\tGame_LowViolence\tcsgo_lv // Perfect World content override\n" +
		"\t\t\tGame\tcsgo/addons/metamod\n"
	if !strings.Contains(out, want) {
		t.Errorf("metamod not inserted right after Game_LowViolence:\n%s", out)
	}
	if strings.Index(out, "csgo/addons/metamod") > strings.Index(out, "\tGame\tcsgo\n") {
		t.Error("metamod must come before Game csgo to load")
	}
	if strings.Count(out, "\n") != strings.Count(stockGameInfo, "\n")+1 {
		t.Error("expected exactly one new line")
	}
}

func TestAddGameInfoSearchPathsIsIdempotent(t *testing.T) {
	paths := []string{"csgo/addons/metamod", "csgo/addons/other"}
	once, _, err := core.AddGameInfoSearchPaths(stockGameInfo, paths)
	if err != nil {
		t.Fatal(err)
	}
	twice, added, err := core.AddGameInfoSearchPaths(once, paths)
	if err != nil || len(added) != 0 || twice != once {
		t.Errorf("second pass changed the file (added %v, err %v)", added, err)
	}
	// Formatting differences (quotes, backslashes, case) still count as present.
	manual := strings.Replace(stockGameInfo, "\t\t\tGame\tcsgo\n", "\t\t\tGame\t\"CSGO\\addons\\metamod\\\"\n\t\t\tGame\tcsgo\n", 1)
	if _, added, _ := core.AddGameInfoSearchPaths(manual, []string{"csgo/addons/metamod"}); len(added) != 0 {
		t.Errorf("didn't recognize an existing metamod line, added %v", added)
	}
}

func TestAddGameInfoSearchPathsKeepsCRLF(t *testing.T) {
	crlf := strings.ReplaceAll(stockGameInfo, "\n", "\r\n")
	out, _, err := core.AddGameInfoSearchPaths(crlf, []string{"csgo/addons/metamod"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(out, "\n") != strings.Count(out, "\r\n") {
		t.Error("mixed line endings after patching a CRLF file")
	}
	if !strings.Contains(out, "\tGame\tcsgo/addons/metamod\r\n") {
		t.Error("metamod line missing")
	}
}

func TestAddGameInfoSearchPathsWithoutLowViolence(t *testing.T) {
	in := strings.Replace(stockGameInfo, "\t\t\tGame_LowViolence\tcsgo_lv // Perfect World content override\n", "", 1)
	out, _, err := core.AddGameInfoSearchPaths(in, []string{"csgo/addons/metamod"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "\t\t\tGame\tcsgo/addons/metamod\n\t\t\tGame\tcsgo\n") {
		t.Errorf("expected metamod just before Game csgo:\n%s", out)
	}
}

func TestAddGameInfoSearchPathsRejectsBrokenFile(t *testing.T) {
	for name, text := range map[string]string{
		"no SearchPaths": `"GameInfo" { FileSystem { } }`,
		"unclosed":       "SearchPaths\n{\n\tGame csgo\n",
	} {
		if _, _, err := core.AddGameInfoSearchPaths(text, []string{"csgo/addons/metamod"}); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
}

func TestEnsureGameInfoSearchPathsRewritesOnlyWhenNeeded(t *testing.T) {
	file := filepath.Join(t.TempDir(), "gameinfo.gi")
	os.WriteFile(file, []byte(stockGameInfo), 0o644)

	added, err := core.EnsureGameInfoSearchPaths(file, []string{"csgo/addons/metamod"})
	if err != nil || len(added) != 1 {
		t.Fatalf("added=%v err=%v", added, err)
	}
	data, _ := os.ReadFile(file)
	if !strings.Contains(string(data), "Game\tcsgo/addons/metamod") {
		t.Fatal("file not patched")
	}

	stat1, _ := os.Stat(file)
	added, err = core.EnsureGameInfoSearchPaths(file, []string{"csgo/addons/metamod"})
	stat2, _ := os.Stat(file)
	if err != nil || len(added) != 0 || !stat1.ModTime().Equal(stat2.ModTime()) {
		t.Errorf("already patched file was rewritten (added=%v err=%v)", added, err)
	}

	// An update puts the stock file back; the next start patches it again.
	os.WriteFile(file, []byte(stockGameInfo), 0o644)
	if added, _ := core.EnsureGameInfoSearchPaths(file, []string{"csgo/addons/metamod"}); len(added) != 1 {
		t.Error("did not re-patch after the file was reset")
	}

	if _, err := core.EnsureGameInfoSearchPaths(filepath.Join(t.TempDir(), "missing.gi"), []string{"x"}); err == nil {
		t.Error("expected an error for a missing file")
	}
}
