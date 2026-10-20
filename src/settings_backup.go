package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/branchkit/branchkit-plugin-browser/wire"
	"github.com/branchkit/plugin-sdk-go"
)

// A copy of the browser extension's settings, kept here so a reinstall can
// get them back.
//
// Why the plugin keeps it: a browser deletes an extension's storage when the
// extension is removed, and Chrome's cloud copy only comes back for the same
// extension id with sync on. The owner lost their site rules that way. The
// extension can always write a backup FILE, but nobody remembers to before an
// uninstall; this copy is the one that exists without being asked for. The
// extension mirrors its durable settings here a couple of seconds after each
// change, and on a fresh install offers to restore them (it asks first).
//
// The document is the extension's format and stays opaque here, except for
// one question: does it hold any settings? An empty copy never replaces a
// non-empty one unless the extension says `force` (the user cleared every
// setting on purpose). The extension already holds that rule; this is the
// second line, so a bug there cannot wipe the copy a fresh install is about
// to be offered.
//
// One slot per browser family: Chrome and Firefox can carry different
// settings, and a fresh Chrome should be offered Chrome's. A family with no
// slot gets the newest copy of any.
//
// Settings carry site patterns, so the file lives in this plugin's own data
// directory, on this machine, owner-only.

// maxSettingsBackupBytes caps one stored document. Chrome's whole sync area
// is ~100 KB and the local settings are small; a megabyte is room for years
// of references and still refuses a runaway writer.
const maxSettingsBackupBytes = 1 << 20

const settingsBackupFileName = "settings-backup.json"

// settingsBackupSlot is one browser family's copy.
type settingsBackupSlot struct {
	SavedAt int64           `json:"saved_at"`
	Backup  json.RawMessage `json:"backup"`
}

// settingsBackupFile is the file's whole content.
type settingsBackupFile struct {
	Slots map[string]settingsBackupSlot `json:"slots"`
}

// settingsBackupMu serialises the file's read-modify-write.
var settingsBackupMu sync.Mutex

// backupRename is the atomic step of a save (test seam: a failing rename must
// leave the previous copy whole).
var backupRename = os.Rename

func settingsBackupPath() string {
	dir := branchkit.PluginDataDir()
	if dir == "" {
		return "" // not launched by the platform: nowhere to keep it
	}
	return filepath.Join(dir, settingsBackupFileName)
}

func validBackupBrowser(b string) bool { return b == "chrome" || b == "firefox" }

// loadSettingsBackups reads the file. A missing file is no copies; an
// unreadable one is an error (a save must not replace what it cannot read).
func loadSettingsBackups(path string) (settingsBackupFile, error) {
	f := settingsBackupFile{Slots: map[string]settingsBackupSlot{}}
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return f, nil
	}
	if err != nil {
		return f, err
	}
	if err := json.Unmarshal(raw, &f); err != nil {
		return f, err
	}
	if f.Slots == nil {
		f.Slots = map[string]settingsBackupSlot{}
	}
	return f, nil
}

// writeFileAtomic writes data to path through a temp file in the same
// directory and a rename, so a reader (or a crash) sees the old file or the
// new one, never half of either.
func writeFileAtomic(path string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*.tmp")
	if err != nil {
		return err
	}
	name := tmp.Name()
	cleanup := func() { _ = os.Remove(name) }
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		cleanup()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		cleanup()
		return err
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return err
	}
	if err := os.Chmod(name, 0o600); err != nil {
		cleanup()
		return err
	}
	if err := backupRename(name, path); err != nil {
		cleanup()
		return err
	}
	return nil
}

// backupSettingsCount is how many settings a document holds, or an error when
// it is not the extension's shape (an object with a settings object).
func backupSettingsCount(doc json.RawMessage) (int, error) {
	var d struct {
		Settings *struct {
			Sync  map[string]json.RawMessage `json:"sync"`
			Local map[string]json.RawMessage `json:"local"`
		} `json:"settings"`
	}
	if err := json.Unmarshal(doc, &d); err != nil {
		return 0, err
	}
	if d.Settings == nil {
		return 0, errors.New("no settings object")
	}
	return len(d.Settings.Sync) + len(d.Settings.Local), nil
}

func (h *Host) handleSettingsBackupPut(_ *http.Request, req *wire.SettingsBackupPutRequest) (wire.Ack, error) {
	if req.ConnID == "" {
		return wire.Ack{}, badRequest("missing conn_id")
	}
	if !validBackupBrowser(req.Browser) {
		return wire.Ack{}, badRequest("browser must be chrome or firefox")
	}
	if len(req.Backup) > maxSettingsBackupBytes {
		return wire.Ack{}, statusError(http.StatusRequestEntityTooLarge, "settings backup is larger than 1 MB")
	}
	count, err := backupSettingsCount(req.Backup)
	if err != nil {
		return wire.Ack{}, badRequest("not a settings backup: " + err.Error())
	}

	h.mu.Lock()
	pluginID := h.state.PluginID
	live := h.state.clientOf(req.ConnID) != ""
	h.mu.Unlock()
	// Same rule as the session: only a connection with a live stream speaks
	// for this browser. The extension mirrors again on its next connect.
	if !live {
		return wire.Ack{}, statusError(http.StatusConflict, "no live stream for conn_id")
	}

	path := settingsBackupPath()
	if path == "" {
		return wire.Ack{}, statusError(http.StatusServiceUnavailable, "no plugin data directory")
	}

	settingsBackupMu.Lock()
	defer settingsBackupMu.Unlock()
	file, err := loadSettingsBackups(path)
	if err != nil {
		branchkit.Logf(pluginID, "[SETTINGS_BACKUP] unreadable %s: %v — refusing to replace it", path, err)
		return wire.Ack{}, err
	}
	if prev, ok := file.Slots[req.Browser]; ok && count == 0 && !req.Force {
		if n, err := backupSettingsCount(prev.Backup); err == nil && n > 0 {
			return wire.Ack{}, statusError(http.StatusConflict, "refusing to replace a backup that has settings with an empty one (send force to clear it)")
		}
	}
	file.Slots[req.Browser] = settingsBackupSlot{SavedAt: time.Now().UnixMilli(), Backup: append(json.RawMessage(nil), req.Backup...)}
	data, err := json.Marshal(file)
	if err != nil {
		return wire.Ack{}, err
	}
	if err := writeFileAtomic(path, data); err != nil {
		branchkit.Logf(pluginID, "[SETTINGS_BACKUP] write failed: %v", err)
		return wire.Ack{}, err
	}
	// An open Backup tab shows the new copy. Off the request: the extension
	// does not wait on a settings re-render.
	h.runAsync(h.refreshSettings)
	return ack()
}

func (h *Host) handleSettingsBackupGet(_ *http.Request, q *wire.SettingsBackupQuery) (wire.SettingsBackupResponse, error) {
	if q.Browser != "" && !validBackupBrowser(q.Browser) {
		return wire.SettingsBackupResponse{}, badRequest("browser must be chrome or firefox")
	}
	path := settingsBackupPath()
	if path == "" {
		return wire.SettingsBackupResponse{Found: false}, nil
	}
	settingsBackupMu.Lock()
	file, err := loadSettingsBackups(path)
	settingsBackupMu.Unlock()
	if err != nil {
		return wire.SettingsBackupResponse{}, err
	}
	browser := ""
	if _, ok := file.Slots[q.Browser]; ok && q.Browser != "" {
		browser = q.Browser
	} else {
		var newest int64 = -1
		for b, s := range file.Slots {
			// Ties go to the alphabetically first family, so the answer is
			// stable.
			if s.SavedAt > newest || (s.SavedAt == newest && b < browser) {
				newest, browser = s.SavedAt, b
			}
		}
	}
	if browser == "" {
		return wire.SettingsBackupResponse{Found: false}, nil
	}
	s := file.Slots[browser]
	return wire.SettingsBackupResponse{Found: true, Browser: browser, SavedAt: s.SavedAt, Backup: s.Backup}, nil
}
