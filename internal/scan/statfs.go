package scan

import "syscall"

// Usage はファイルシステムの容量。
type Usage struct {
	Total uint64 `json:"total"`
	Free  uint64 `json:"free"`
	Used  uint64 `json:"used"`
}

// Statfs は path を含むファイルシステムの容量を返す。Free は root 以外が使える量。
func Statfs(path string) (Usage, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return Usage{}, err
	}
	bs := uint64(st.Bsize) //nolint:unconvert,gosec // Bsize の型は OS ごとに違い、負にはならない。
	u := Usage{
		Total: uint64(st.Blocks) * bs,
		Free:  uint64(st.Bavail) * bs,
	}
	used := uint64(st.Blocks-st.Bfree) * bs //nolint:unconvert
	u.Used = used
	return u, nil
}
