package encode

import (
	"os"
	"path/filepath"
)

// renameVariantDirs normalizes ffmpeg's numeric variant directories:
// 0 -> low, 1 -> high, and init_<n>.mp4 -> init.mp4. Playlist URI references
// are rewritten accordingly.
func renameVariantDirs(outDir string) error {
	for i, spec := range Renditions {
		numDir := filepath.Join(outDir, itoa(i))
		dstDir := filepath.Join(outDir, spec.Name)
		if err := os.MkdirAll(dstDir, 0o755); err != nil {
			return err
		}
		entries, err := os.ReadDir(numDir)
		if err != nil {
			return err
		}
		for _, e := range entries {
			src := filepath.Join(numDir, e.Name())
			name := e.Name()
			if len(name) >= 5 && name[:5] == "init_" {
				name = "init.mp4"
			}
			dst := filepath.Join(dstDir, name)
			if err := os.Rename(src, dst); err != nil {
				return err
			}
		}
		if err := os.RemoveAll(numDir); err != nil {
			return err
		}
		playlist := filepath.Join(dstDir, "index.m3u8")
		data, err := os.ReadFile(playlist)
		if err != nil {
			return err
		}
		old := []byte("init_" + itoa(i) + ".mp4")
		data = bytesReplaceAll(data, old, []byte("init.mp4"))
		if err := os.WriteFile(playlist, data, 0o644); err != nil {
			return err
		}
	}
	return nil
}

func bytesReplaceAll(s, old, new []byte) []byte {
	var out []byte
	for {
		i := bytesIndex(s, old)
		if i < 0 {
			return append(out, s...)
		}
		out = append(out, s[:i]...)
		out = append(out, new...)
		s = s[i+len(old):]
	}
}

func bytesIndex(s, sep []byte) int {
	for i := 0; i+len(sep) <= len(s); i++ {
		match := true
		for j := range sep {
			if s[i+j] != sep[j] {
				match = false
				break
			}
		}
		if match {
			return i
		}
	}
	return -1
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [8]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}
