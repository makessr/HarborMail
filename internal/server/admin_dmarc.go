package server

import (
	"archive/zip"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"errors"
	"io"
	"net/http"
	"net/netip"
	"strings"
)

const dmarcUploadLimit = 4 << 20
const dmarcXMLLimit = 8 << 20

type dmarcReport struct {
	Key      string `json:"key"`
	Domain   string `json:"domain"`
	Reporter string `json:"reporter"`
	ID       string `json:"reportID"`
	Start    int64  `json:"start"`
	End      int64  `json:"end"`
	Total    int64  `json:"total"`
	Aligned  int64  `json:"aligned"`
}

type dmarcSource struct {
	Address     string `xml:"row>source_ip" json:"address"`
	Count       int64  `xml:"row>count" json:"count"`
	Disposition string `xml:"row>policy_evaluated>disposition" json:"disposition"`
	DKIM        string `xml:"row>policy_evaluated>dkim" json:"dkim"`
	SPF         string `xml:"row>policy_evaluated>spf" json:"spf"`
}

func readDMARCXML(raw []byte) ([]byte, error) {
	var source io.Reader = bytes.NewReader(raw)
	if bytes.HasPrefix(raw, []byte{0x1f, 0x8b}) {
		gzipFile, err := gzip.NewReader(source)
		if err != nil {
			return nil, err
		}
		defer gzipFile.Close()
		source = gzipFile
	} else if bytes.HasPrefix(raw, []byte("PK\x03\x04")) {
		archive, err := zip.NewReader(bytes.NewReader(raw), int64(len(raw)))
		if err != nil {
			return nil, err
		}
		var entry *zip.File
		for _, file := range archive.File {
			if file.FileInfo().IsDir() || !strings.HasSuffix(strings.ToLower(file.Name), ".xml") {
				continue
			}
			if entry != nil || file.UncompressedSize64 > dmarcXMLLimit {
				return nil, errors.New("ZIP must contain one bounded XML report")
			}
			entry = file
		}
		if entry == nil {
			return nil, errors.New("ZIP does not contain an XML report")
		}
		xmlEntry, err := entry.Open()
		if err != nil {
			return nil, err
		}
		defer xmlEntry.Close()
		source = xmlEntry
	}
	xmlBytes, err := io.ReadAll(io.LimitReader(source, dmarcXMLLimit+1))
	if len(xmlBytes) > dmarcXMLLimit {
		return nil, errors.New("DMARC XML too large")
	}
	return xmlBytes, err
}

func parseDMARCReport(raw []byte) (dmarcReport, []dmarcSource, error) {
	xmlBytes, err := readDMARCXML(raw)
	if err != nil {
		return dmarcReport{}, nil, err
	}
	var document struct {
		XMLName  xml.Name      `xml:"feedback"`
		Reporter string        `xml:"report_metadata>org_name"`
		ID       string        `xml:"report_metadata>report_id"`
		Start    int64         `xml:"report_metadata>date_range>begin"`
		End      int64         `xml:"report_metadata>date_range>end"`
		Domain   string        `xml:"policy_published>domain"`
		Sources  []dmarcSource `xml:"record"`
	}
	if err := xml.Unmarshal(xmlBytes, &document); err != nil {
		return dmarcReport{}, nil, err
	}
	report := dmarcReport{
		Domain:   strings.TrimSuffix(strings.ToLower(strings.TrimSpace(document.Domain)), "."),
		Reporter: strings.TrimSpace(document.Reporter),
		ID:       strings.TrimSpace(document.ID),
		Start:    document.Start, End: document.End,
	}
	if !validDNSName(report.Domain) || !strings.Contains(report.Domain, ".") ||
		len(report.Reporter) == 0 || len(report.Reporter) > 254 ||
		len(report.ID) == 0 || len(report.ID) > 254 ||
		report.Start < 0 || report.End <= report.Start || report.End > 253402300799 ||
		len(document.Sources) == 0 || len(document.Sources) > 10000 {
		return dmarcReport{}, nil, errors.New("invalid DMARC report metadata")
	}
	for i := range document.Sources {
		source := &document.Sources[i]
		ip, err := netip.ParseAddr(strings.TrimSpace(source.Address))
		if err != nil || ip.Zone() != "" || source.Count < 0 || source.Count > 1_000_000_000 ||
			!validDMARCState(source.SPF) || !validDMARCState(source.DKIM) ||
			(source.Disposition != "none" && source.Disposition != "quarantine" && source.Disposition != "reject") {
			return dmarcReport{}, nil, errors.New("invalid DMARC record")
		}
		source.Address = ip.Unmap().String()
		report.Total += source.Count
		if source.SPF == "pass" || source.DKIM == "pass" {
			report.Aligned += source.Count
		}
	}
	key := sha256.Sum256([]byte(report.Domain + "\x00" + strings.ToLower(report.Reporter) + "\x00" + report.ID))
	report.Key = hex.EncodeToString(key[:])
	return report, document.Sources, nil
}

func validDMARCState(state string) bool {
	return state == "pass" || state == "fail"
}

var dmarcImportSlots = make(chan struct{}, 2)

func (a *app) adminImportDMARCReport(w http.ResponseWriter, r *http.Request, _ string) {
	select {
	case dmarcImportSlots <- struct{}{}:
		defer func() { <-dmarcImportSlots }()
	default:
		fail(w, http.StatusTooManyRequests, "报告导入繁忙，请稍后重试")
		return
	}
	payload, err := io.ReadAll(http.MaxBytesReader(w, r.Body, dmarcUploadLimit))
	if err != nil {
		fail(w, http.StatusBadRequest, "请选择 4 MiB 以内的 DMARC 报告")
		return
	}
	report, sources, err := parseDMARCReport(payload)
	if err != nil {
		fail(w, http.StatusBadRequest, "无法读取 DMARC 聚合报告，请选择 XML、GZIP 或包含一份 XML 的 ZIP")
		return
	}
	details, err := json.Marshal(sources)
	if letterError(w, err) {
		return
	}
	query := `INSERT INTO dmarc_reports(report_key,domain,reporter,report_id,period_start,period_end,total,aligned,details)
		VALUES(?,?,?,?,?,?,?,?,?) ON CONFLICT(report_key) DO NOTHING`
	if a.dbDriver == "mysql" {
		query = `INSERT INTO dmarc_reports(report_key,domain,reporter,report_id,period_start,period_end,total,aligned,details)
			VALUES(?,?,?,?,?,?,?,?,?) ON DUPLICATE KEY UPDATE report_key=report_key`
	}
	if _, err := a.db.ExecContext(r.Context(), query, report.Key, report.Domain, report.Reporter, report.ID,
		report.Start, report.End, report.Total, report.Aligned, string(details)); letterError(w, err) {
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (a *app) adminDMARCReports(w http.ResponseWriter, r *http.Request, _ string) {
	if key := r.URL.Query().Get("key"); key != "" {
		if _, err := hex.DecodeString(key); len(key) != 64 || err != nil {
			fail(w, http.StatusBadRequest, "无效的报告编号")
			return
		}
		var details []byte
		err := a.db.QueryRowContext(r.Context(), "SELECT details FROM dmarc_reports WHERE report_key=?", key).Scan(&details)
		if !letterError(w, err) {
			reply(w, http.StatusOK, json.RawMessage(details))
		}
		return
	}
	query := "SELECT report_key,domain,reporter,report_id,period_start,period_end,total,aligned FROM dmarc_reports"
	var args []any
	if domain := strings.TrimSpace(strings.ToLower(r.URL.Query().Get("domain"))); domain != "" {
		if !validDNSName(domain) {
			fail(w, http.StatusBadRequest, "请输入有效邮件域名")
			return
		}
		query += " WHERE domain=?"
		args = append(args, domain)
	}
	rows, err := a.db.QueryContext(r.Context(), query+" ORDER BY period_end DESC,report_key LIMIT 100", args...)
	if letterError(w, err) {
		return
	}
	defer rows.Close()
	reports := make([]dmarcReport, 0)
	for rows.Next() {
		var report dmarcReport
		if letterError(w, rows.Scan(&report.Key, &report.Domain, &report.Reporter, &report.ID, &report.Start,
			&report.End, &report.Total, &report.Aligned)) {
			return
		}
		reports = append(reports, report)
	}
	if !letterError(w, rows.Err()) {
		reply(w, http.StatusOK, reports)
	}
}
