package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

type FileEntry struct {
	RelPath string      `json:"rel_path"`
	Mode    os.FileMode `json:"mode"`
	SHA256  string      `json:"sha256"`
	Content []byte      `json:"content"`
}

type RemoteBundle struct {
	Version       string            `json:"version"`
	CreatedAt     time.Time         `json:"created_at"`
	SourceWorkdir string            `json:"source_workdir"`
	Session       *ParsedSession    `json:"session"`
	Files         []FileEntry       `json:"files"`
	ProjectRules  []FileEntry       `json:"project_rules"`
	GlobalRules   map[string]string `json:"global_rules"`
}

func slimSessionForRemote(ps *ParsedSession) *ParsedSession {
	if ps == nil {
		return nil
	}
	cp := *ps
	// Only send the active post-compaction context (`CompactSummary` + `PostCompact`)
	// instead of duplicating ancient pre-compaction `AllTurns` (which can be 60-200 MB
	// in long-lived project transcripts).
	cp.AllTurns = nil
	post := make([]SessionTurn, len(ps.PostCompact))
	copy(post, ps.PostCompact)
	cp.PostCompact = post

	// Ensure total JSON size of the active turns fits comfortably within public paste limits (~2.5 MB uncompressed -> ~350 KB gzipped)
	if rawJSON, err := json.Marshal(&cp); err == nil && len(rawJSON) > 2500000 {
		for i := range cp.PostCompact {
			if len(cp.PostCompact[i].RawOutput) > 16384 {
				cp.PostCompact[i].RawOutput = cp.PostCompact[i].RawOutput[:16384] + "\n...[truncated oversized raw output for remote transfer]"
			}
		}
		if rawJSON2, err := json.Marshal(&cp); err == nil && len(rawJSON2) > 2500000 && len(cp.PostCompact) > 40 {
			budget := 2200000
			used := len(cp.CompactSummary)
			keepFrom := len(cp.PostCompact)
			for i := len(cp.PostCompact) - 1; i >= 0; i-- {
				sz := len(cp.PostCompact[i].Text) + len(cp.PostCompact[i].RawOutput) + 128
				if used+sz > budget && keepFrom < len(cp.PostCompact) {
					break
				}
				used += sz
				keepFrom = i
			}
			cp.PostCompact = cp.PostCompact[keepFrom:]
		}
	}
	return &cp
}

func pushRemoteSession(agent, query, storage, localDir string) (string, error) {
	ps, err := findAndParseSessionForAction(agent, query, true)
	if err != nil {
		return "", err
	}

	cwd, _ := os.Getwd()
	files := collectModifiedWorkspaceFiles(cwd, ps.ModifiedFiles)
	projectRules := collectProjectRuleFiles(cwd)
	globalRules := collectGlobalRuleFiles()

	bundle := RemoteBundle{
		Version:       version,
		CreatedAt:     time.Now(),
		SourceWorkdir: cwd,
		Session:       slimSessionForRemote(ps),
		Files:         files,
		ProjectRules:  projectRules,
		GlobalRules:   globalRules,
	}

	encryptedB64, hexKey, err := compressAndEncryptBundle(&bundle)
	if err != nil {
		return "", err
	}

	storage = strings.ToLower(strings.TrimSpace(storage))
	if storage == "" {
		storage = "auto"
	}

	if storage == "local_dir" || localDir != "" {
		if localDir == "" {
			localDir = os.Getenv("BATON_STORAGE_PATH")
		}
		if localDir == "" {
			home, _ := os.UserHomeDir()
			localDir = filepath.Join(home, ".baton_vault")
		}
		if err := os.MkdirAll(localDir, 0700); err != nil {
			return "", err
		}
		fileName := fmt.Sprintf("%s-%d.baton", ps.Agent, time.Now().Unix())
		fullPath := filepath.Join(localDir, fileName)
		if err := os.WriteFile(fullPath, []byte(encryptedB64), 0600); err != nil {
			return "", err
		}
		return fmt.Sprintf("baton:file:%s#%s", fullPath, hexKey), nil
	}

	type provider struct {
		name string
		fn   func(string) (string, error)
	}

	var chain []provider
	switch storage {
	case "pasters", "paste.rs":
		chain = []provider{{"pasters", uploadPasteRS}}
	case "dpaste", "dpaste.org":
		chain = []provider{{"dpaste", uploadDpaste}}
	case "termbin", "termbin.com":
		chain = []provider{{"termbin", uploadTermbin}}
	default:
		chain = []provider{
			{"pasters", uploadPasteRS},
			{"dpaste", uploadDpaste},
			{"termbin", uploadTermbin},
		}
	}

	var lastErr error
	for _, p := range chain {
		id, err := p.fn(encryptedB64)
		if err == nil && id != "" {
			var urlToken string
			switch p.name {
			case "pasters":
				urlToken = fmt.Sprintf("https://paste.rs/%s", id)
			case "dpaste":
				if strings.HasPrefix(id, "http") {
					urlToken = id
				} else {
					urlToken = fmt.Sprintf("https://dpaste.org/%s/raw", id)
				}
			case "termbin":
				urlToken = fmt.Sprintf("https://termbin.com/%s", id)
			default:
				urlToken = fmt.Sprintf("baton:%s:%s", p.name, id)
			}
			cacheUploadedSnapshot(urlToken, encryptedB64)
			return fmt.Sprintf("%s#%s", urlToken, hexKey), nil
		}
		lastErr = err
	}
	return "", fmt.Errorf("all paste providers failed (last error: %v)", lastErr)
}

func localSnapshotCachePath(beforeHash string) string {
	cacheDir := filepath.Join(filepath.Dir(aliasesFilePath()), "vault_cache")
	_ = os.MkdirAll(cacheDir, 0700)
	return filepath.Join(cacheDir, sha256Hex([]byte(strings.TrimSpace(beforeHash)))+".baton")
}

func cacheUploadedSnapshot(beforeHash, encryptedB64 string) {
	_ = os.WriteFile(localSnapshotCachePath(beforeHash), []byte(encryptedB64), 0600)
}

func saveImportedSessionToLocalIndex(ps *ParsedSession) {
	if ps == nil {
		return
	}
	importDir := filepath.Join(filepath.Dir(aliasesFilePath()), "imported")
	if err := os.MkdirAll(importDir, 0700); err != nil {
		return
	}
	safeID := filepath.Base(ps.SessionID)
	if safeID == "" || safeID == "." || safeID == "/" {
		safeID = fmt.Sprintf("%s-%d", ps.Agent, time.Now().Unix())
	}
	importPath := filepath.Join(importDir, safeID+".baton.json")
	psCopy := *ps
	if len(psCopy.AllTurns) == 0 && len(psCopy.PostCompact) > 0 {
		psCopy.AllTurns = psCopy.PostCompact
	}
	if cwd, err := os.Getwd(); err == nil && cwd != "" {
		psCopy.CWD = cwd
	}
	psCopy.ModTime = time.Now()
	if data, err := json.MarshalIndent(&psCopy, "", "  "); err == nil {
		_ = os.WriteFile(importPath, data, 0600)
		aliases := loadAliases()
		title := strings.TrimSpace(ps.Title)
		if title == "" && ps.CWD != "" {
			title = filepath.Base(ps.CWD)
		}
		if title == "" || title == "." || title == "/" {
			title = safeID
		}
		aliases[importPath] = title
		_ = saveAliases(aliases)
	}
}

func pullRemoteSession(token string) (string, error) {
	cleanupOldHandoffs()
	token = strings.TrimSpace(token)

	hashIdx := strings.IndexByte(token, '#')
	if hashIdx < 0 {
		return "", fmt.Errorf("missing decryption key after '#' in URL/token")
	}
	hexKey := token[hashIdx+1:]
	beforeHash := token[:hashIdx]

	var encryptedB64 string
	var err error

	if strings.HasPrefix(beforeHash, "https://") || strings.HasPrefix(beforeHash, "http://") {
		encryptedB64, err = downloadRawHTTP(beforeHash)
	} else {
		if !strings.HasPrefix(beforeHash, "baton:") {
			return "", fmt.Errorf("invalid token or URL format (expected https://...#<key> or baton:<provider>:<id>#<key>)")
		}
		prefix := beforeHash[len("baton:"):]
		colonIdx := strings.IndexByte(prefix, ':')
		if colonIdx < 0 {
			return "", fmt.Errorf("missing provider and id in token")
		}
		provider := prefix[:colonIdx]
		id := prefix[colonIdx+1:]

		switch provider {
		case "file":
			data, rErr := os.ReadFile(id)
			if rErr != nil {
				return "", rErr
			}
			encryptedB64 = string(data)
		case "pasters":
			encryptedB64, err = downloadRawHTTP("https://paste.rs/" + id)
		case "dpaste":
			rawURL := "https://dpaste.org/" + id + "/raw"
			if strings.HasPrefix(id, "http") {
				rawURL = id
			}
			encryptedB64, err = downloadRawHTTP(rawURL)
		case "termbin":
			encryptedB64, err = downloadRawHTTP("https://termbin.com/" + id)
		default:
			return "", fmt.Errorf("unknown storage provider: %s", provider)
		}
	}
	if err != nil {
		// Fallback to local vault_cache if this machine pushed or previously fetched this URL
		if cached, cErr := os.ReadFile(localSnapshotCachePath(beforeHash)); cErr == nil && len(cached) > 0 {
			encryptedB64 = string(cached)
			err = nil
		}
	} else if encryptedB64 != "" {
		cacheUploadedSnapshot(beforeHash, encryptedB64)
	}
	if err != nil {
		return "", fmt.Errorf("failed to download snapshot: %w", err)
	}

	bundle, err := decryptAndDecompressBundle(strings.TrimSpace(encryptedB64), hexKey)
	if err != nil {
		return "", err
	}
	if bundle.Session != nil && len(bundle.Session.AllTurns) == 0 && len(bundle.Session.PostCompact) > 0 {
		bundle.Session.AllTurns = bundle.Session.PostCompact
	}
	saveImportedSessionToLocalIndex(bundle.Session)

	cwd, _ := os.Getwd()
	var restoredFiles []string
	var backedUpFiles []string

	restoreEntry := func(fe FileEntry) {
		cleanRel := filepath.Clean(fe.RelPath)
		if strings.HasPrefix(cleanRel, "..") || filepath.IsAbs(cleanRel) {
			return
		}
		target := filepath.Join(cwd, cleanRel)
		_ = os.MkdirAll(filepath.Dir(target), 0755)

		if existing, err := os.ReadFile(target); err == nil {
			existingSum := sha256Hex(existing)
			if existingSum != fe.SHA256 {
				bakPath := target + ".baton.bak"
				_ = os.WriteFile(bakPath, existing, 0644)
				backedUpFiles = append(backedUpFiles, cleanRel+".baton.bak")
			}
		}
		mode := fe.Mode
		if mode == 0 {
			mode = 0644
		}
		if err := os.WriteFile(target, fe.Content, mode); err == nil {
			restoredFiles = append(restoredFiles, cleanRel)
		}
	}

	for _, fe := range bundle.Files {
		restoreEntry(fe)
	}
	for _, fe := range bundle.ProjectRules {
		restoreEntry(fe)
	}

	handoffDir, err := writeHandoffDirectory(bundle.Session, bundle.GlobalRules)
	if err != nil {
		return "", err
	}

	var header strings.Builder
	header.WriteString("# REMOTE SESSION RESTORED\n\n")
	if len(restoredFiles) > 0 {
		header.WriteString("## Restored Workspace Files:\n")
		for _, f := range restoredFiles {
			header.WriteString(fmt.Sprintf("- `%s`\n", f))
		}
		header.WriteString("\n")
	}
	if len(backedUpFiles) > 0 {
		header.WriteString("## Local Backups Created Before Overwrite:\n")
		for _, b := range backedUpFiles {
			header.WriteString(fmt.Sprintf("- `%s`\n", b))
		}
		header.WriteString("\n")
	}

	header.WriteString(formatHandoffSummary(bundle.Session, handoffDir, bundle.GlobalRules))
	return header.String(), nil
}

func collectModifiedWorkspaceFiles(cwd string, agentModified []string) []FileEntry {
	var orderedRels []string
	relSet := make(map[string]bool)
	addRel := func(rel string) {
		rel = strings.TrimSpace(rel)
		if rel != "" && !relSet[rel] && !strings.HasPrefix(rel, ".git") && !strings.HasPrefix(rel, ".fslckout") {
			relSet[rel] = true
			orderedRels = append(orderedRels, rel)
		}
	}

	for _, p := range agentModified {
		if rel := toRelIfInside(cwd, p); rel != "" {
			addRel(rel)
		}
	}

	// Git (tracked modified files; untracked files created by the agent are already in agentModified)
	if _, err := os.Stat(filepath.Join(cwd, ".git")); err == nil {
		if out, err := exec.Command("git", "status", "--porcelain", "-uno").Output(); err == nil {
			for _, line := range strings.Split(string(out), "\n") {
				if len(line) > 3 {
					path := strings.TrimSpace(line[3:])
					if idx := strings.Index(path, " -> "); idx >= 0 {
						path = path[idx+4:]
					}
					path = strings.Trim(path, `"`)
					addRel(path)
				}
			}
		}
	}

	// Fossil (tracked modified files via `fossil changes`)
	if _, err := os.Stat(filepath.Join(cwd, ".fslckout")); err == nil {
		if out, err := exec.Command("fossil", "changes").Output(); err == nil {
			for _, line := range strings.Split(string(out), "\n") {
				fields := strings.Fields(line)
				if len(fields) >= 2 {
					addRel(fields[len(fields)-1])
				}
			}
		}
	}

	// SVN (tracked modified files via `svn status -q`)
	if _, err := os.Stat(filepath.Join(cwd, ".svn")); err == nil {
		if out, err := exec.Command("svn", "status", "-q").Output(); err == nil {
			for _, line := range strings.Split(string(out), "\n") {
				fields := strings.Fields(line)
				if len(fields) >= 2 {
					addRel(fields[len(fields)-1])
				}
			}
		}
	}

	var entries []FileEntry
	totalBytes := 0
	const maxTotalFileBytes = 1500 * 1024
	for _, rel := range orderedRels {
		full := filepath.Join(cwd, rel)
		info, err := os.Stat(full)
		if err != nil || info.IsDir() || info.Size() == 0 || info.Size() > 512*1024 {
			continue
		}
		if totalBytes+int(info.Size()) > maxTotalFileBytes {
			continue
		}
		data, err := os.ReadFile(full)
		if err != nil {
			continue
		}
		// Skip compiled binaries/non-text blobs containing NUL bytes
		probeLen := len(data)
		if probeLen > 512 {
			probeLen = 512
		}
		if bytes.IndexByte(data[:probeLen], 0) >= 0 {
			continue
		}
		totalBytes += len(data)
		entries = append(entries, FileEntry{
			RelPath: rel,
			Mode:    info.Mode(),
			SHA256:  sha256Hex(data),
			Content: data,
		})
	}
	return entries
}

func collectProjectRuleFiles(cwd string) []FileEntry {
	candidates := []string{
		"CLAUDE.md",
		"AGENTS.md",
		"GEMINI.md",
		".cursorrules",
		".windsurfrules",
	}
	rulesGlob, _ := filepath.Glob(filepath.Join(cwd, ".agents", "rules", "*.md"))
	for _, g := range rulesGlob {
		if rel, err := filepath.Rel(cwd, g); err == nil {
			candidates = append(candidates, rel)
		}
	}

	var out []FileEntry
	for _, rel := range candidates {
		full := filepath.Join(cwd, rel)
		info, err := os.Stat(full)
		if err != nil || info.IsDir() || info.Size() == 0 || info.Size() > 512*1024 {
			continue
		}
		data, err := os.ReadFile(full)
		if err != nil {
			continue
		}
		out = append(out, FileEntry{
			RelPath: rel,
			Mode:    info.Mode(),
			SHA256:  sha256Hex(data),
			Content: data,
		})
	}
	return out
}

func collectGlobalRuleFiles() map[string]string {
	home, _ := os.UserHomeDir()
	paths := map[string]string{
		"global_CLAUDE.md": filepath.Join(home, ".claude", "CLAUDE.md"),
		"global_GEMINI.md": filepath.Join(home, ".gemini", "GEMINI.md"),
		"global_AGENTS.md": filepath.Join(home, ".gemini", "config", "AGENTS.md"),
	}
	res := make(map[string]string)
	for name, p := range paths {
		if data, err := os.ReadFile(p); err == nil && len(bytes.TrimSpace(data)) > 0 && len(data) <= 256*1024 {
			res[name] = string(data)
		}
	}
	return res
}

func toRelIfInside(cwd, p string) string {
	if !filepath.IsAbs(p) {
		clean := filepath.Clean(p)
		if !strings.HasPrefix(clean, "..") {
			return clean
		}
		return ""
	}
	rel, err := filepath.Rel(cwd, p)
	if err != nil || strings.HasPrefix(rel, "..") {
		return ""
	}
	return rel
}

// compressAndEncryptBundle packs the RemoteBundle into an in-memory tar.gz
// stream using Go's built-in archive/tar and compress/gzip packages (compiled
// directly into the binary for Windows, macOS, and Linux without external CLI
// tools), and encrypts it with AES-256-GCM.
func compressAndEncryptBundle(b *RemoteBundle) (string, string, error) {
	var gzBuf bytes.Buffer
	gz, err := gzip.NewWriterLevel(&gzBuf, gzip.BestCompression)
	if err != nil {
		return "", "", err
	}
	tw := tar.NewWriter(gz)

	// Write bundle.json with raw file contents stored as separate tar entries
	// to avoid +33% JSON Base64 overhead and maximize solid LZ77 compression.
	metaBundle := *b
	metaBundle.Files = make([]FileEntry, len(b.Files))
	for i, f := range b.Files {
		metaBundle.Files[i] = FileEntry{RelPath: f.RelPath, Mode: f.Mode, SHA256: f.SHA256}
	}
	metaBundle.ProjectRules = make([]FileEntry, len(b.ProjectRules))
	for i, f := range b.ProjectRules {
		metaBundle.ProjectRules[i] = FileEntry{RelPath: f.RelPath, Mode: f.Mode, SHA256: f.SHA256}
	}

	metaBytes, err := json.Marshal(&metaBundle)
	if err != nil {
		return "", "", err
	}
	if err := writeTarEntry(tw, "bundle.json", metaBytes, 0600); err != nil {
		return "", "", err
	}
	for i, f := range b.Files {
		if err := writeTarEntry(tw, fmt.Sprintf("files/%d", i), f.Content, int64(f.Mode)); err != nil {
			return "", "", err
		}
	}
	for i, f := range b.ProjectRules {
		if err := writeTarEntry(tw, fmt.Sprintf("rules/%d", i), f.Content, int64(f.Mode)); err != nil {
			return "", "", err
		}
	}

	if err := tw.Close(); err != nil {
		return "", "", err
	}
	if err := gz.Close(); err != nil {
		return "", "", err
	}

	key := make([]byte, 32)
	if _, err := io.ReadFull(rand.Reader, key); err != nil {
		return "", "", err
	}

	block, err := aes.NewCipher(key)
	if err != nil {
		return "", "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", "", err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", "", err
	}

	ciphertext := gcm.Seal(nonce, nonce, gzBuf.Bytes(), nil)
	return base64.StdEncoding.EncodeToString(ciphertext), hex.EncodeToString(key), nil
}

func writeTarEntry(tw *tar.Writer, name string, data []byte, mode int64) error {
	if mode == 0 {
		mode = 0644
	}
	hdr := &tar.Header{
		Name:    name,
		Mode:    mode,
		Size:    int64(len(data)),
		ModTime: time.Now(),
	}
	if err := tw.WriteHeader(hdr); err != nil {
		return err
	}
	_, err := tw.Write(data)
	return err
}

func decryptAndDecompressBundle(b64Data, hexKey string) (*RemoteBundle, error) {
	key, err := hex.DecodeString(strings.TrimSpace(hexKey))
	if err != nil || len(key) != 32 {
		return nil, fmt.Errorf("invalid 256-bit decryption key")
	}
	ciphertext, err := base64.StdEncoding.DecodeString(strings.TrimSpace(b64Data))
	if err != nil {
		return nil, fmt.Errorf("base64 decode error: %w", err)
	}

	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	if len(ciphertext) < gcm.NonceSize() {
		return nil, fmt.Errorf("corrupted encrypted payload")
	}

	nonce := ciphertext[:gcm.NonceSize()]
	enc := ciphertext[gcm.NonceSize():]
	compressed, err := gcm.Open(nil, nonce, enc, nil)
	if err != nil {
		return nil, fmt.Errorf("AES-256-GCM authentication failed: %w", err)
	}

	gz, err := gzip.NewReader(bytes.NewReader(compressed))
	if err != nil {
		return nil, err
	}
	defer gz.Close()

	decompressed, err := io.ReadAll(gz)
	if err != nil {
		return nil, err
	}

	// Read in-memory tar stream (with fallback to legacy plain JSON)
	tr := tar.NewReader(bytes.NewReader(decompressed))
	var bundle RemoteBundle
	fileBlobs := make(map[string][]byte)
	isTar := false

	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			isTar = false
			break
		}
		isTar = true
		data, err := io.ReadAll(tr)
		if err != nil {
			return nil, err
		}
		if hdr.Name == "bundle.json" {
			if err := json.Unmarshal(data, &bundle); err != nil {
				return nil, err
			}
		} else {
			fileBlobs[hdr.Name] = data
		}
	}

	if isTar && bundle.Session != nil {
		for i := range bundle.Files {
			if content, ok := fileBlobs[fmt.Sprintf("files/%d", i)]; ok {
				bundle.Files[i].Content = content
			}
		}
		for i := range bundle.ProjectRules {
			if content, ok := fileBlobs[fmt.Sprintf("rules/%d", i)]; ok {
				bundle.ProjectRules[i].Content = content
			}
		}
		return &bundle, nil
	}

	if err := json.Unmarshal(decompressed, &bundle); err != nil {
		return nil, err
	}
	return &bundle, nil
}

func uploadPasteRS(payload string) (string, error) {
	client := &http.Client{Timeout: 30 * time.Second}
	req, err := http.NewRequest("POST", "https://paste.rs/", strings.NewReader(payload))
	if err != nil {
		return "", err
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("paste.rs returned HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	u := strings.TrimSpace(string(body))
	parts := strings.Split(strings.TrimRight(u, "/"), "/")
	return parts[len(parts)-1], nil
}

func uploadDpaste(payload string) (string, error) {
	client := &http.Client{Timeout: 30 * time.Second}
	form := url.Values{}
	form.Set("content", payload)
	form.Set("format", "url")
	form.Set("expires", "604800")

	resp, err := client.PostForm("https://dpaste.org/api/", form)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("dpaste.org returned HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	u := strings.Trim(strings.TrimSpace(string(body)), `"`)
	parts := strings.Split(strings.TrimRight(u, "/"), "/")
	return parts[len(parts)-1], nil
}

func uploadTermbin(payload string) (string, error) {
	conn, err := net.DialTimeout("tcp", "termbin.com:9999", 15*time.Second)
	if err != nil {
		return "", err
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(30 * time.Second))
	if _, err := io.WriteString(conn, payload+"\n"); err != nil {
		return "", err
	}
	resp, err := io.ReadAll(conn)
	if err != nil {
		return "", err
	}
	u := strings.TrimSpace(strings.TrimRight(string(resp), "\x00\r\n"))
	if !strings.HasPrefix(u, "http") {
		return "", fmt.Errorf("unexpected termbin response: %s", u)
	}
	parts := strings.Split(strings.TrimRight(u, "/"), "/")
	return parts[len(parts)-1], nil
}

func downloadRawHTTP(rawURL string) (string, error) {
	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Get(rawURL)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("HTTP %d fetching %s", resp.StatusCode, rawURL)
	}
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	return string(data), nil
}

func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}
