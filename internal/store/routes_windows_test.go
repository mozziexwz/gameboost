package store

import (
	"reflect"
	"strings"
	"testing"
)

func TestProcessFoldersPersistAndCompileRecursiveRules(t *testing.T) {
	layout := NewLayout(t.TempDir())
	settings := DefaultSettings()
	settings.Processes = []string{"Single.exe"}
	settings.ProcessFolders = []string{` C:/Games/My Game/./ `, `c:\games\my game`, `D:\`, `\\example\games`}
	if err := layout.SaveSettings(settings); err != nil {
		t.Fatal(err)
	}
	loaded, err := layout.LoadSettings()
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{`C:\Games\My Game`, `D:\`, `\\example\games`}; !reflect.DeepEqual(loaded.ProcessFolders, want) {
		t.Fatalf("folders = %v, want %v", loaded.ProcessFolders, want)
	}
	if programs, hosts := loaded.RouteRule(); programs != `Single.exe;C:\Games\My Game\*;D:\*;\\example\games\*` || hosts != "*" {
		t.Fatalf("process rule = %q, %q", programs, hosts)
	}
	loaded.Mode = ModeIP
	loaded.Targets = []string{"192.0.2.2"}
	if programs, hosts := loaded.RouteRule(); programs != "*" || hosts != "192.0.2.2" {
		t.Fatalf("IP rule = %q, %q", programs, hosts)
	}
}

func TestProcessFoldersRejectInvalidRules(t *testing.T) {
	for _, value := range []string{`relative\path`, `C:games`, `C:\games\*`, `C:\games;other`, `C:\games,file`, `C:\games:stream`, `\\.\PhysicalDrive0`, `\\server`, `\\`} {
		if _, err := NormalizeProcessFolders([]string{value}); err == nil {
			t.Errorf("accepted an invalid folder %q", value)
		}
	}
	settings := DefaultSettings()
	settings.Processes = []string{strings.Repeat("a", 950) + ".exe"}
	settings.ProcessFolders = []string{`C:\` + strings.Repeat("b", 100)}
	if err := settings.Validate(); err == nil {
		t.Fatal("accepted a process and folder list that would be truncated by ProxyBridge")
	}
}
