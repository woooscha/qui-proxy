package proxy

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
)

// The threshold is decimal TB; sysfs reports capacities in 512-byte sectors.
const minDiskBytes uint64 = 10_000_000_000_000

type disk struct{ source, destination string }

func (h *handler) discoverDisks() ([]disk, error) {
	resolvedRoot, err := filepath.EvalSymlinks(h.cfg.DisksRoot)
	if err != nil || resolvedRoot != h.cfg.DisksRoot {
		return nil, errors.New("disk root unavailable or unsafe")
	}
	entries, err := os.ReadDir(h.cfg.DisksRoot)
	if err != nil {
		return nil, fmt.Errorf("reading disk directory: %w", err)
	}
	f, err := os.Open("/proc/self/mountinfo")
	if err != nil {
		return nil, fmt.Errorf("opening mount information: %w", err)
	}
	defer closeWithLog(f)
	mounts := make(map[string][]mount)
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		sep := slices.Index(fields, "-")
		if len(fields) < 6 || sep < 6 || sep+1 >= len(fields) {
			continue
		}
		name := strings.NewReplacer(`\040`, " ", `\011`, "\t", `\012`, "\n", `\134`, `\`).Replace(fields[4])
		mounts[name] = append(mounts[name], mount{device: fields[2], filesystem: fields[sep+1], readonly: slices.Contains(strings.Split(fields[5], ","), "ro")})
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("reading mount information: %w", err)
	}
	var disks []disk
	for _, entry := range entries {
		name := entry.Name()
		if !diskName(name) {
			continue
		}
		root := filepath.Join(h.cfg.DisksRoot, name)
		if !entry.IsDir() || entry.Type()&os.ModeSymlink != 0 {
			return nil, fmt.Errorf("unsafe disk directory %s", name)
		}
		records := mounts[root]
		if len(records) != 1 {
			return nil, fmt.Errorf("disk %s is not a unique mount", name)
		}
		sectors, err := os.ReadFile(filepath.Join("/sys/dev/block", records[0].device, "size"))
		if err != nil {
			return nil, fmt.Errorf("reading disk %s capacity: %w", name, err)
		}
		eligible, err := eligibleCapacity(string(sectors))
		if err != nil {
			return nil, fmt.Errorf("parsing disk %s capacity: %w", name, err)
		}
		if !eligible {
			continue
		}
		if !records[0].readonly || records[0].filesystem != "xfs" {
			return nil, fmt.Errorf("disk %s is not a read-only xfs mount", name)
		}
		data := filepath.Join(root, "data")
		resolved, err := filepath.EvalSymlinks(data)
		if err != nil || resolved != data {
			return nil, fmt.Errorf("disk %s data directory unavailable or unsafe", name)
		}
		var rootFS, dataFS syscall.Statfs_t
		if err := syscall.Statfs(root, &rootFS); err != nil {
			return nil, fmt.Errorf("checking disk %s: %w", name, err)
		}
		if err := syscall.Statfs(data, &dataFS); err != nil {
			return nil, fmt.Errorf("checking disk %s data: %w", name, err)
		}
		if uint64(rootFS.Type) != xfsMagic || rootFS.Fsid != dataFS.Fsid {
			return nil, fmt.Errorf("disk %s data is not on its xfs mount", name)
		}
		disks = append(disks, disk{source: data, destination: "/" + name})
	}
	if len(disks) == 0 {
		return nil, errors.New("no eligible disks")
	}
	return disks, nil
}

type mount struct {
	device, filesystem string
	readonly           bool
}

func eligibleCapacity(value string) (bool, error) {
	sectors, err := strconv.ParseUint(strings.TrimSpace(value), 10, 64)
	if err != nil {
		return false, err
	}
	return sectors > minDiskBytes/512, nil
}

func diskName(name string) bool {
	n, ok := strings.CutPrefix(name, "disk")
	if !ok || n == "" || n[0] == '0' {
		return false
	}
	for _, c := range n {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

// present checks each component without following symlinks, including when a
// different file in the same torrent is missing from this disk.
func present(root, rel string) (bool, error) {
	current := root
	for part := range strings.SplitSeq(rel, string(filepath.Separator)) {
		current = filepath.Join(current, part)
		st, err := os.Lstat(current)
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		if err != nil {
			return false, fmt.Errorf("checking torrent path: %w", err)
		}
		if st.Mode()&os.ModeSymlink != 0 {
			return false, errors.New("symlink in torrent path")
		}
	}
	return true, nil
}

func (h *handler) verify(save string, files []file) (string, string, error) {
	disks, err := h.discoverDisks()
	if err != nil {
		return "", "", err
	}
	return h.selectDisk(save, files, disks, h.verifyCandidate)
}

func (h *handler) selectDisk(save string, files []file, disks []disk, verifyCandidate func(string, string, []file) error) (string, string, error) {
	if len(files) == 0 {
		return "", "", errors.New("empty torrent file list")
	}
	for _, f := range files {
		if f.Name == "" || filepath.IsAbs(f.Name) || filepath.Clean(f.Name) != f.Name || f.Name == ".." || strings.HasPrefix(f.Name, "../") || f.Size == nil || *f.Size < 0 || f.Progress != 1 {
			return "", "", errors.New("invalid or incomplete torrent file")
		}
	}
	var chosen disk
	for _, d := range disks {
		physical, err := h.translate(save, d.source)
		if err != nil {
			return "", "", err
		}
		relSave, err := filepath.Rel(d.source, physical)
		if err != nil {
			return "", "", fmt.Errorf("checking save path: %w", err)
		}
		found := 0
		for _, f := range files {
			exists, err := present(d.source, filepath.Join(relSave, f.Name))
			if err != nil {
				return "", "", err
			}
			if exists {
				found++
			}
		}
		if found == 0 {
			continue
		}
		if found != len(files) || chosen.source != "" {
			return "", "", errors.New("torrent files are spread across disks or ambiguous")
		}
		if err := verifyCandidate(d.source, physical, files); err != nil {
			return "", "", err
		}
		chosen = d
	}
	if chosen.source == "" {
		return "", "", errors.New("torrent not found on an eligible disk")
	}
	return chosen.source, chosen.destination, nil
}

func (h *handler) verifyCandidate(root, physical string, files []file) error {
	var fs syscall.Statfs_t
	if err := syscall.Statfs(root, &fs); err != nil {
		return fmt.Errorf("checking filesystem: %w", err)
	}
	return h.verifyFiles(root, physical, files, func(target string) error {
		var fileFS syscall.Statfs_t
		if err := syscall.Statfs(target, &fileFS); err != nil {
			return fmt.Errorf("checking torrent filesystem: %w", err)
		}
		if fileFS.Fsid != fs.Fsid || uint64(fileFS.Type) != xfsMagic {
			return errors.New("torrent file is not on the configured xfs filesystem")
		}
		return nil
	})
}
