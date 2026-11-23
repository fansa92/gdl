package download

import (
	"encoding/json"
	"os"
	"time"
)

// chunk 表示文件中的一段字节范围。
type chunk struct {
	Start int64 `json:"start"`
	End   int64 `json:"end"` // 含端点；-1 表示长度未知
	Done  int64 `json:"done"`
}

func (c chunk) total() int64 {
	if c.End < 0 {
		return -1
	}
	return c.End - c.Start + 1
}

func (c chunk) complete() bool {
	t := c.total()
	return t >= 0 && c.Done >= t
}

// state 是断点续传的元数据，保存在 `<输出文件>.part.json`。
type state struct {
	URL       string    `json:"url"`
	Size      int64     `json:"size"` // -1 表示未知
	ETag      string    `json:"etag,omitempty"`
	Modified  string    `json:"last_modified,omitempty"`
	Chunks    []chunk   `json:"chunks"`
	UpdatedAt time.Time `json:"updated_at"`
}

// loadState 读取并校验断点状态。返回 nil 表示需要重新下载。
func (d *downloader) loadState(inf *info) *state {
	data, err := os.ReadFile(d.statePath)
	if err != nil {
		return nil
	}
	var st state
	if err := json.Unmarshal(data, &st); err != nil {
		return nil
	}
	if st.URL != d.opt.URL || len(st.Chunks) == 0 {
		return nil
	}
	// 资源发生变化则作废旧进度
	if st.Size >= 0 && inf.size >= 0 && st.Size != inf.size {
		return nil
	}
	if inf.etag != "" && st.ETag != "" && inf.etag != st.ETag {
		return nil
	}
	if inf.modified != "" && st.Modified != "" && inf.modified != st.Modified {
		return nil
	}

	fi, err := os.Stat(d.tmp)
	if err != nil {
		return nil
	}
	flen := fi.Size()

	if len(st.Chunks) > 1 {
		// 分块模式文件已预分配为完整长度，长度不足说明数据被截断过
		if inf.size >= 0 && flen != inf.size {
			return nil
		}
	} else {
		// 单流模式：以磁盘上实际存在的连续数据为准
		st.Chunks[0].Done = flen
		if st.Chunks[0].End >= 0 && flen > st.Chunks[0].total() {
			return nil
		}
	}

	for _, c := range st.Chunks {
		if c.Start < 0 || c.End < -1 || c.Done < 0 {
			return nil
		}
		if c.End >= 0 && c.Start > c.End {
			return nil
		}
		if c.End >= 0 && c.Done > c.total() {
			return nil
		}
	}
	return &st
}

// saveState 原子地写入断点状态。
func (d *downloader) saveState(chunks []chunk, inf *info) {
	st := state{
		URL:       d.opt.URL,
		Size:      inf.size,
		ETag:      inf.etag,
		Modified:  inf.modified,
		Chunks:    append([]chunk(nil), chunks...),
		UpdatedAt: time.Now(),
	}
	data, err := json.Marshal(&st)
	if err != nil {
		return
	}
	tmp := d.statePath + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return
	}
	if err := os.Rename(tmp, d.statePath); err != nil {
		os.Remove(tmp)
	}
}
