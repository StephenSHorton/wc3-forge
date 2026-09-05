package casc

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// wc3InstallDefault is the canonical install root for the current OS. The
// tests skip when nothing's found there (a fresh dev machine without WC3
// is a normal state) — set WC3FORGE_WC3_PATH to override.
var wc3InstallDefault = func() string {
	if runtime.GOOS == "windows" {
		return `C:\Program Files (x86)\Warcraft III`
	}
	return "/Applications/Warcraft III"
}()

func wc3InstallPath(t *testing.T) string {
	t.Helper()
	p := os.Getenv("WC3FORGE_WC3_PATH")
	if p == "" {
		p = wc3InstallDefault
	}
	// .build.info is what CascLib looks for to recognize a CASC root; if
	// it's missing, the test wouldn't get past Open anyway.
	if _, err := os.Stat(filepath.Join(p, ".build.info")); err != nil {
		t.Skipf("WC3 install not found at %q (set WC3FORGE_WC3_PATH to override)", p)
	}
	return p
}

func TestOpen(t *testing.T) {
	s, err := Open(wc3InstallPath(t))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if s.handle == 0 {
		t.Fatalf("nil storage handle")
	}
}

func TestEnumerate(t *testing.T) {
	s, err := Open(wc3InstallPath(t))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()

	// Enumeration goes through the platform raw-op helpers (rawFindFirstFile
	// /NextFile/Close), which Open already bound cross-platform (Open ->
	// loadLib -> bindCASCLib). Using them — instead of re-binding through a
	// dlopen handle — keeps this test working on Windows, where the call
	// layer is syscall.LazyProc.Call rather than purego.
	//
	// CASC_FIND_DATA from CascLib.h — findDataSize bytes. Most of that is
	// szFileName (findDataNameOff..+findDataNameLen) which we read as result.
	var data [findDataSize]byte
	hFind := rawFindFirstFile(s.handle, "*", &data)
	if hFind == 0 || hFind == ^uintptr(0) {
		t.Fatalf("CascFindFirstFile failed (handle=%x)", hFind)
	}
	defer rawFindClose(hFind)

	count := 0
	for {
		name := readCString(data[findDataNameOff : findDataNameOff+findDataNameLen])
		lname := strings.ToLower(name)
		if strings.Contains(lname, "footman.mdx") || strings.Contains(lname, "miscdata.txt") || strings.Contains(lname, "units.slk") || strings.Contains(lname, "teamcolor00.blp") {
			t.Logf("[%d] %s", count, name)
		}
		count++
		if count > 100000 {
			t.Logf("... (capped at %d entries)", count)
			break
		}
		if !rawFindNextFile(hFind, &data) {
			break
		}
	}
	t.Logf("Total CASC entries: %d", count)
}

func readCString(b []byte) string {
	for i, c := range b {
		if c == 0 {
			return string(b[:i])
		}
	}
	return string(b)
}

func TestMountPrefixes_NoTilesetMatchesLegacy(t *testing.T) {
	sd := mountPrefixes(false, 0)
	if len(sd) != len(sdCascPrefixes) {
		t.Fatalf("SD no-tileset len=%d, want %d", len(sd), len(sdCascPrefixes))
	}
	for i := range sd {
		if sd[i] != sdCascPrefixes[i] {
			t.Fatalf("SD no-tileset [%d]=%q, want %q", i, sd[i], sdCascPrefixes[i])
		}
	}
	hd := mountPrefixes(true, 0)
	if len(hd) != len(hdCascPrefixes) {
		t.Fatalf("HD no-tileset len=%d, want %d", len(hd), len(hdCascPrefixes))
	}
	for i := range hd {
		if hd[i] != hdCascPrefixes[i] {
			t.Fatalf("HD no-tileset [%d]=%q, want %q", i, hd[i], hdCascPrefixes[i])
		}
	}
}

func TestMountPrefixes_TilesetLetter(t *testing.T) {
	hd := mountPrefixes(true, 'A')
	if hd[0] != "war3.w3mod:_hd.w3mod:_tilesets/a.w3mod:" {
		t.Fatalf("HD tileset-A first prefix = %q", hd[0])
	}
	if hd[1] != "war3.w3mod:_tilesets/a.w3mod:" {
		t.Fatalf("HD tileset-A second prefix = %q", hd[1])
	}
	// Legacy HD prefixes still follow the tileset mounts.
	if hd[2] != hdCascPrefixes[0] {
		t.Fatalf("HD tileset-A third prefix = %q, want legacy first %q", hd[2], hdCascPrefixes[0])
	}

	sd := mountPrefixes(false, 'X')
	if sd[0] != "war3.w3mod:_tilesets/x.w3mod:" {
		t.Fatalf("SD tileset-X first prefix = %q", sd[0])
	}
	if sd[1] != "war3.w3mod:_hd.w3mod:_tilesets/x.w3mod:" {
		t.Fatalf("SD tileset-X second prefix = %q", sd[1])
	}
}

func TestTilesetMountLetter(t *testing.T) {
	cases := []struct {
		in   byte
		want string
	}{
		{0, ""},
		{'A', "a"},
		{'a', "a"},
		{'X', "x"},
		{'L', "l"},
		{255, ""},
		{'1', ""},
	}
	for _, tc := range cases {
		if got := tilesetMountLetter(tc.in); got != tc.want {
			t.Errorf("tilesetMountLetter(%q)=%q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestReadCanonicalAssets(t *testing.T) {
	s, err := Open(wc3InstallPath(t))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()

	// Files we expect to find in any vaguely-recent WC3 install. Lowercase
	// paths, forward-slash. SLK + INI + a guaranteed stock unit + a stock
	// texture cover the four flavours of asset mdx-m3-viewer will request.
	cases := []struct {
		name    string
		minSize int
	}{
		// Caller-relative (no prefix) — ReadFile prepends mount prefixes.
		{"units/human/footman/footman.mdx", 1000},
		{"units\\human\\footman\\footman.mdx", 1000},
		{"ReplaceableTextures/Water/Water00.dds", 1000},
		// Note: not every well-known asset lives under war3.w3mod:; the
		// mount prefix list in casc.go will need to grow as misses surface.
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			data, ok, err := s.ReadFile(c.name)
			if err != nil {
				t.Fatalf("ReadFile: %v", err)
			}
			if !ok {
				t.Fatalf("%s not present in CASC", c.name)
			}
			if len(data) < c.minSize {
				t.Errorf("%s too small: %d bytes < expected %d", c.name, len(data), c.minSize)
			}
			t.Logf("%s: %d bytes", c.name, len(data))
		})
	}
}

func TestReadWaterTexture_WithTilesetMount(t *testing.T) {
	s, err := Open(wc3InstallPath(t))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()

	// Ashenvale ('A') and Reforged ('X') both have a tileset-specific water
	// mount in current CASC; Lordaeron ('L') does not and must still fall
	// through to the generic ReplaceableTextures/Water/Water00.dds.
	for _, ts := range []byte{'A', 'X', 'L'} {
		s.SetTileset(ts)
		data, ok, err := s.ReadFile("ReplaceableTextures/Water/Water00.dds")
		if err != nil {
			t.Fatalf("tileset %c: ReadFile: %v", ts, err)
		}
		if !ok {
			t.Fatalf("tileset %c: Water00.dds missing", ts)
		}
		if len(data) < 1000 {
			t.Errorf("tileset %c: Water00.dds too small: %d bytes", ts, len(data))
		}
		if string(data[:4]) != "DDS " {
			t.Errorf("tileset %c: Water00.dds magic %q, want DDS ", ts, data[:4])
		}
	}
}
