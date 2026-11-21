package server

// finding 报告导出（0.15.29）：GET /api/exploration/findings/{id}/report.docx
// 把 reporter agent 写回的 Markdown 报告转成 Word 文档下发。转换器是 docx.go 的
// 纯标准库实现（零依赖）；报告为空时 404（没报告的漏洞没有可导出的东西）。

import (
	"net/http"
	"strconv"
)

func (s *Server) findingReportDocx(w http.ResponseWriter, r *http.Request) {
	id := int64(atoiDefault(r.PathValue("id"), 0))
	if id <= 0 {
		writeErr(w, 400, "bad finding id")
		return
	}
	f, err := s.m.pg.GetFinding(id)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	if f == nil {
		writeErr(w, 404, "finding not found")
		return
	}
	if f.Report == "" {
		writeErr(w, 404, "该漏洞还没有报告（reporter 未写入或报告为空）")
		return
	}
	body, err := renderDocx(f.Report)
	if err != nil {
		writeErr(w, 500, "docx 渲染失败: "+err.Error())
		return
	}
	filename := "finding-" + strconv.FormatInt(id, 10) + "-report.docx"
	w.Header().Set("Content-Type", "application/vnd.openxmlformats-officedocument.wordprocessingml.document")
	w.Header().Set("Content-Disposition", `attachment; filename="`+filename+`"`)
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	_, _ = w.Write(body)
}
