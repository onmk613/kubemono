package artifact

import (
	"encoding/binary"
	"fmt"
	"io"
	"os"
)

// ExecInfo 是从可执行文件头部读出的平台信息。
type ExecInfo struct {
	// Format 为空表示不是可识别的可执行文件。
	Format string
	// OS 为空表示无法判断。
	OS string
	// Arch 为空表示无法判断。
	Arch string
}

const elfMagic = "\x7fELF"

var (
	elfMachines = map[uint16]string{
		3:   "386",
		62:  "amd64",
		183: "arm64",
		40:  "arm",
		243: "riscv64",
	}
	machoCPUs = map[uint32]string{
		0x01000007: "amd64",
		0x0100000c: "arm64",
		0x00000007: "386",
		0x0000000c: "arm",
	}
)

// SniffExecutable 只读头部，判断可执行文件的格式与目标平台。
func SniffExecutable(r io.ReaderAt) ExecInfo {
	buf := make([]byte, 64)
	n, err := r.ReadAt(buf, 0)
	if err != nil && n < 4 {
		return ExecInfo{}
	}
	buf = buf[:n]
	switch {
	case len(buf) >= 4 && string(buf[:4]) == elfMagic:
		return elfHeader(buf)
	case len(buf) >= 8 && (binary.LittleEndian.Uint32(buf) == 0xfeedfacf || binary.LittleEndian.Uint32(buf) == 0xfeedface):
		return machoHeader(buf)
	case len(buf) >= 4 && binary.BigEndian.Uint32(buf) == 0xcafebabe:
		return ExecInfo{Format: "Mach-O 通用二进制", OS: "darwin"}
	case len(buf) >= 2 && buf[0] == 'M' && buf[1] == 'Z':
		return ExecInfo{Format: "PE", OS: "windows"}
	default:
		return ExecInfo{}
	}
}

// CheckExecutable 校验 path 是不是目标 OS/架构的可执行文件。
// 只读文件头，不执行它；无法判断架构时以“格式对”为准。
func CheckExecutable(p, osName, arch string) error {
	f, err := os.Open(p)
	if err != nil {
		return fmt.Errorf("打开可执行文件 %s 失败: %w", p, err)
	}
	defer func() { _ = f.Close() }()
	h := SniffExecutable(f)
	if h.Format == "" {
		return fmt.Errorf("%s 不是可识别的可执行文件（ELF/Mach-O/PE）", p)
	}
	if h.OS != "" && osName != "" && h.OS != osName {
		return fmt.Errorf("%s 是 %s 可执行文件，目标 OS 是 %s", p, h.OS, osName)
	}
	if h.Arch != "" && arch != "" && h.Arch != arch {
		return fmt.Errorf("%s 是 %s/%s 可执行文件，目标平台是 %s/%s", p, h.OS, h.Arch, osName, arch)
	}
	return nil
}

func elfHeader(buf []byte) ExecInfo {
	h := ExecInfo{Format: "ELF", OS: "linux"}
	if len(buf) < 20 {
		return h
	}
	var machine uint16
	if buf[5] == 2 { // EI_DATA = ELFDATA2MSB
		machine = binary.BigEndian.Uint16(buf[18:20])
	} else {
		machine = binary.LittleEndian.Uint16(buf[18:20])
	}
	if arch, ok := elfMachines[machine]; ok {
		h.Arch = arch
	}
	return h
}

func machoHeader(buf []byte) ExecInfo {
	h := ExecInfo{Format: "Mach-O", OS: "darwin"}
	if len(buf) < 8 {
		return h
	}
	if arch, ok := machoCPUs[binary.LittleEndian.Uint32(buf[4:8])]; ok {
		h.Arch = arch
	}
	return h
}
