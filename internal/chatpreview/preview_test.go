package chatpreview

import (
	"archive/zip"
	"bytes"
	"errors"
	"fmt"
	"strings"
	"testing"
)

func TestBuildCSVCreatesBoundedTable(t *testing.T) {
	content := []byte("Company;Ticker;Note\nAlphabet;GOOGL;=SUM(1,1)\nNvidia;NVDA;fast\n")
	preview, err := Build("companies.csv", "text/csv", bytes.NewReader(content), int64(len(content)), "")
	if err != nil {
		t.Fatal(err)
	}
	if preview.Kind != "table" || strings.Join(preview.Columns, ",") != "Company,Ticker,Note" {
		t.Fatalf("columns=%v kind=%q", preview.Columns, preview.Kind)
	}
	if len(preview.Rows) != 2 || preview.Rows[0][0] != "Alphabet" || preview.Rows[0][2] != "=SUM(1,1)" {
		t.Fatalf("rows=%v", preview.Rows)
	}
	if preview.TotalRows != 2 || !preview.TotalRowsExact {
		t.Fatalf("total rows=%d exact=%t", preview.TotalRows, preview.TotalRowsExact)
	}
	if len(preview.RowNumbers) != 2 || preview.RowNumbers[0] != 2 {
		t.Fatalf("row numbers=%v", preview.RowNumbers)
	}
}

func TestBuildCSVPreservesFirstRowWhenThereIsNoHeader(t *testing.T) {
	content := []byte("Alice,Paris\nBob,London\n")
	preview, err := Build("people.csv", "text/csv", bytes.NewReader(content), int64(len(content)), "")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(preview.Columns, ",") != "A,B" {
		t.Fatalf("columns=%v", preview.Columns)
	}
	if len(preview.Rows) != 2 || strings.Join(preview.Rows[0], ",") != "Alice,Paris" {
		t.Fatalf("rows=%v", preview.Rows)
	}
	if preview.TotalRows != 2 || len(preview.RowNumbers) != 2 || preview.RowNumbers[0] != 1 {
		t.Fatalf("total=%d row numbers=%v", preview.TotalRows, preview.RowNumbers)
	}
}

func TestBuildTextLimitsRowsAndCells(t *testing.T) {
	lines := make([]string, MaxRows+1)
	for index := range lines {
		lines[index] = fmt.Sprintf("line-%03d", index+1)
	}
	lines[0] = strings.Repeat("x", MaxCellRunes+10)
	content := []byte(strings.Join(lines, "\n"))
	preview, err := Build("report.txt", "text/plain", bytes.NewReader(content), int64(len(content)), "")
	if err != nil {
		t.Fatal(err)
	}
	if len(preview.Rows) != MaxRows || !preview.TruncatedRows || !preview.TruncatedCells {
		t.Fatalf("rows=%d truncated_rows=%t truncated_cells=%t", len(preview.Rows), preview.TruncatedRows, preview.TruncatedCells)
	}
	if preview.TotalRows != MaxRows+1 || !preview.TotalRowsExact {
		t.Fatalf("total rows=%d exact=%t", preview.TotalRows, preview.TotalRowsExact)
	}
	if !strings.HasSuffix(preview.Rows[0][0], "…") {
		t.Fatalf("first cell was not visibly truncated: %q", preview.Rows[0][0])
	}
}

func TestBuildXLSXReadsSheetsAndCachedFormulaValues(t *testing.T) {
	content := testWorkbook(t, map[string]string{
		"xl/workbook.xml":            `<?xml version="1.0"?><workbook xmlns:r="http://schemas.openxmlformats.org/officeDocument/2006/relationships"><sheets><sheet name="Overview" sheetId="1" r:id="rId1"/><sheet name="Data" sheetId="2" r:id="rId2"/></sheets></workbook>`,
		"xl/_rels/workbook.xml.rels": `<?xml version="1.0"?><Relationships><Relationship Id="rId1" Target="worksheets/sheet1.xml"/><Relationship Id="rId2" Target="worksheets/sheet2.xml"/></Relationships>`,
		"xl/sharedStrings.xml":       `<?xml version="1.0"?><sst><si><t>Company</t></si><si><t>Value</t></si><si><r><t>Alpha</t></r><r><t>bet</t></r></si></sst>`,
		"xl/worksheets/sheet1.xml":   `<?xml version="1.0"?><worksheet><sheetData><row r="1"><c r="A1" t="s"><v>0</v></c><c r="B1" t="s"><v>1</v></c></row><row r="2"><c r="A2" t="s"><v>2</v></c><c r="B2"><f>SUM(1,1)</f><v>2</v></c></row></sheetData></worksheet>`,
		"xl/worksheets/sheet2.xml":   `<?xml version="1.0"?><worksheet><sheetData><row r="1"><c r="A1" t="inlineStr"><is><t>Status</t></is></c><c r="B1" t="inlineStr"><is><t>Count</t></is></c></row><row r="5"><c r="A5" t="inlineStr"><is><t>Ready</t></is></c><c r="B5"><v>36</v></c></row></sheetData></worksheet>`,
	})
	preview, err := Build("report.xlsx", "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet", bytes.NewReader(content), int64(len(content)), "")
	if err != nil {
		t.Fatal(err)
	}
	if preview.Sheet != "Overview" || strings.Join(preview.Sheets, ",") != "Overview,Data" {
		t.Fatalf("sheet=%q sheets=%v", preview.Sheet, preview.Sheets)
	}
	if strings.Join(preview.Columns, ",") != "Company,Value" || len(preview.Rows) != 1 || strings.Join(preview.Rows[0], ",") != "Alphabet,2" {
		t.Fatalf("columns=%v rows=%v", preview.Columns, preview.Rows)
	}
	if strings.Contains(strings.Join(preview.Rows[0], ","), "SUM") {
		t.Fatalf("formula source leaked into preview: %v", preview.Rows)
	}
	if preview.TotalRows != 1 || !preview.TotalRowsExact {
		t.Fatalf("total rows=%d exact=%t", preview.TotalRows, preview.TotalRowsExact)
	}

	dataPreview, err := Build("report.xlsx", "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet", bytes.NewReader(content), int64(len(content)), "Data")
	if err != nil {
		t.Fatal(err)
	}
	if dataPreview.Sheet != "Data" || len(dataPreview.Rows) != 1 || dataPreview.Rows[0][0] != "Ready" || dataPreview.RowNumbers[0] != 5 {
		t.Fatalf("data preview=%+v", dataPreview)
	}
}

func TestBuildXLSXReportsTotalRowsBeyondPreview(t *testing.T) {
	var worksheet strings.Builder
	worksheet.WriteString(`<worksheet><sheetData><row r="1"><c r="A1" t="inlineStr"><is><t>No.</t></is></c><c r="B1" t="inlineStr"><is><t>Tracking Number</t></is></c></row>`)
	for index := 1; index <= 592; index++ {
		fmt.Fprintf(&worksheet, `<row r="%d"><c r="A%d"><v>%d</v></c><c r="B%d" t="inlineStr"><is><t>P%013d</t></is></c></row>`, index+1, index+1, index, index+1, index)
	}
	worksheet.WriteString(`</sheetData></worksheet>`)
	content := testWorkbook(t, map[string]string{
		"xl/workbook.xml":            `<workbook xmlns:r="urn:r"><sheets><sheet name="Tracking" r:id="rId1"/></sheets></workbook>`,
		"xl/_rels/workbook.xml.rels": `<Relationships><Relationship Id="rId1" Target="worksheets/sheet1.xml"/></Relationships>`,
		"xl/worksheets/sheet1.xml":   worksheet.String(),
	})
	preview, err := Build("tracking.xlsx", "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet", bytes.NewReader(content), int64(len(content)), "")
	if err != nil {
		t.Fatal(err)
	}
	if len(preview.Rows) != MaxRows || preview.TotalRows != 592 || !preview.TotalRowsExact || !preview.TruncatedRows {
		t.Fatalf("rows=%d total=%d exact=%t truncated=%t", len(preview.Rows), preview.TotalRows, preview.TotalRowsExact, preview.TruncatedRows)
	}
}

func TestBuildXLSXRejectsMissingAndUnsafeSheets(t *testing.T) {
	content := testWorkbook(t, map[string]string{
		"xl/workbook.xml":            `<workbook xmlns:r="urn:r"><sheets><sheet name="Data" r:id="rId1"/></sheets></workbook>`,
		"xl/_rels/workbook.xml.rels": `<Relationships><Relationship Id="rId1" Target="../../escape.xml"/></Relationships>`,
		"xl/worksheets/sheet1.xml":   `<worksheet><sheetData/></worksheet>`,
	})
	_, err := Build("report.xlsx", "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet", bytes.NewReader(content), int64(len(content)), "")
	if !errors.Is(err, ErrInvalid) {
		t.Fatalf("unsafe relationship error=%v", err)
	}

	valid := testWorkbook(t, map[string]string{
		"xl/workbook.xml":            `<workbook xmlns:r="urn:r"><sheets><sheet name="Data" r:id="rId1"/></sheets></workbook>`,
		"xl/_rels/workbook.xml.rels": `<Relationships><Relationship Id="rId1" Target="worksheets/sheet1.xml"/></Relationships>`,
		"xl/worksheets/sheet1.xml":   `<worksheet><sheetData/></worksheet>`,
	})
	_, err = Build("report.xlsx", "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet", bytes.NewReader(valid), int64(len(valid)), "Missing")
	if !errors.Is(err, ErrSheetMissing) {
		t.Fatalf("missing sheet error=%v", err)
	}
}

func TestBuildRejectsUnsupportedArtifacts(t *testing.T) {
	content := []byte("document")
	_, err := Build("report.pdf", "application/pdf", bytes.NewReader(content), int64(len(content)), "")
	if !errors.Is(err, ErrUnsupported) {
		t.Fatalf("unsupported error=%v", err)
	}
}

func testWorkbook(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buffer bytes.Buffer
	archive := zip.NewWriter(&buffer)
	for name, content := range files {
		entry, err := archive.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := entry.Write([]byte(content)); err != nil {
			t.Fatal(err)
		}
	}
	if err := archive.Close(); err != nil {
		t.Fatal(err)
	}
	return buffer.Bytes()
}
