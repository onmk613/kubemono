package version

// 构建元数据，在构建时注入：
var (
	// Commit 是构建二进制所用的 git commit。
	Commit = ""
	// BuildDate 是 UTC 构建时间戳。
	BuildDate = ""
)

type Info struct {
	Release    string `json:"release"`
	Kubernetes string `json:"kubernetes"`
	GoVersion  string `json:"goVersion"`
	Commit     string `json:"commit,omitempty"`
	BuildDate  string `json:"buildDate,omitempty"`
	OS         string `json:"os"`
	Arch       string `json:"arch"`
}
