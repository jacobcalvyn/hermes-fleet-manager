package chatpreview

import (
	"archive/zip"
	"bufio"
	"bytes"
	"encoding/csv"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"unicode/utf8"
)

const (
	MaxRows            = 50
	MaxColumns         = 50
	MaxCellRunes       = 512
	maximumSourceBytes = 8 << 20
	maximumTextLine    = 1 << 20
	maximumZipEntries  = 2048
	maximumZipBytes    = 128 << 20
	maximumXMLBytes    = 32 << 20
	maximumSharedItems = 200000
)

var (
	ErrUnsupported  = errors.New("artifact preview is not supported")
	ErrInvalid      = errors.New("artifact preview content is invalid")
	ErrSheetMissing = errors.New("artifact preview sheet was not found")
)

type Preview struct {
	Kind             string     `json:"kind"`
	Columns          []string   `json:"columns"`
	Rows             [][]string `json:"rows"`
	RowNumbers       []int      `json:"row_numbers,omitempty"`
	Sheets           []string   `json:"sheets,omitempty"`
	Sheet            string     `json:"sheet,omitempty"`
	TotalRows        int        `json:"total_rows"`
	TotalRowsExact   bool       `json:"total_rows_exact"`
	TruncatedRows    bool       `json:"truncated_rows"`
	TruncatedColumns bool       `json:"truncated_columns"`
	TruncatedCells   bool       `json:"truncated_cells"`
}

func Build(name, mediaType string, source io.ReaderAt, size int64, sheet string) (Preview, error) {
	if source == nil || size < 0 {
		return Preview{}, fmt.Errorf("%w: source is unavailable", ErrInvalid)
	}
	mediaType = strings.ToLower(strings.TrimSpace(strings.Split(mediaType, ";")[0]))
	extension := strings.ToLower(filepath.Ext(name))
	switch {
	case mediaType == "text/csv" || extension == ".csv":
		if sheet != "" {
			return Preview{}, fmt.Errorf("%w: CSV artifacts do not contain sheets", ErrSheetMissing)
		}
		return buildCSV(source, size)
	case mediaType == "text/plain" || extension == ".txt":
		if sheet != "" {
			return Preview{}, fmt.Errorf("%w: text artifacts do not contain sheets", ErrSheetMissing)
		}
		return buildText(source, size)
	case mediaType == "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet" || extension == ".xlsx":
		return buildXLSX(source, size, sheet)
	default:
		return Preview{}, ErrUnsupported
	}
}

func buildCSV(source io.ReaderAt, size int64) (Preview, error) {
	data, sourceTruncated, err := readSection(source, size, maximumSourceBytes)
	if err != nil {
		return Preview{}, fmt.Errorf("%w: read CSV: %v", ErrInvalid, err)
	}
	if bytes.IndexByte(data, 0) >= 0 || !utf8.Valid(data) {
		return Preview{}, fmt.Errorf("%w: CSV must be UTF-8 text", ErrInvalid)
	}
	reader := csv.NewReader(bytes.NewReader(data))
	reader.Comma = detectDelimiter(data)
	reader.FieldsPerRecord = -1
	rows := make([][]string, 0, MaxRows+2)
	rowNumbers := make([]int, 0, MaxRows+2)
	totalRows := 0
	truncatedColumns := false
	truncatedCells := false
	for {
		record, readErr := reader.Read()
		if errors.Is(readErr, io.EOF) {
			break
		}
		if readErr != nil {
			if sourceTruncated && totalRows > 0 {
				break
			}
			return Preview{}, fmt.Errorf("%w: parse CSV: %v", ErrInvalid, readErr)
		}
		totalRows++
		if len(rows) >= MaxRows+2 {
			continue
		}
		processed, columnsCut, cellsCut := boundRow(record)
		rows = append(rows, processed)
		rowNumbers = append(rowNumbers, totalRows)
		truncatedColumns = truncatedColumns || columnsCut
		truncatedCells = truncatedCells || cellsCut
	}
	preview := finalizeTable(rows, rowNumbers, totalRows, !sourceTruncated)
	preview.TruncatedRows = preview.TruncatedRows || sourceTruncated
	preview.TruncatedColumns = truncatedColumns
	preview.TruncatedCells = truncatedCells
	return preview, nil
}

func buildText(source io.ReaderAt, size int64) (Preview, error) {
	readerSize := size
	truncated := false
	if readerSize > maximumSourceBytes {
		readerSize = maximumSourceBytes
		truncated = true
	}
	reader := io.NewSectionReader(source, 0, readerSize)
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 64<<10), maximumTextLine)
	rows := make([][]string, 0, MaxRows)
	rowNumbers := make([]int, 0, MaxRows)
	truncatedCells := false
	lineNumber := 0
	for scanner.Scan() {
		lineNumber++
		if len(rows) >= MaxRows {
			continue
		}
		if !utf8.Valid(scanner.Bytes()) {
			return Preview{}, fmt.Errorf("%w: text must be UTF-8", ErrInvalid)
		}
		value, cut := boundCell(scanner.Text())
		rows = append(rows, []string{value})
		rowNumbers = append(rowNumbers, lineNumber)
		truncatedCells = truncatedCells || cut
	}
	if err := scanner.Err(); err != nil {
		return Preview{}, fmt.Errorf("%w: parse text: %v", ErrInvalid, err)
	}
	return Preview{
		Kind: "table", Columns: []string{"Text"}, Rows: rows, RowNumbers: rowNumbers,
		TotalRows: lineNumber, TotalRowsExact: !truncated,
		TruncatedRows: truncated || lineNumber > len(rows), TruncatedCells: truncatedCells,
	}, nil
}

func readSection(source io.ReaderAt, size, maximum int64) ([]byte, bool, error) {
	readSize := size
	truncated := false
	if readSize > maximum {
		readSize = maximum
		truncated = true
	}
	data, err := io.ReadAll(io.NewSectionReader(source, 0, readSize))
	return data, truncated, err
}

func detectDelimiter(data []byte) rune {
	line := data
	if newline := bytes.IndexByte(line, '\n'); newline >= 0 {
		line = line[:newline]
	}
	candidates := []rune{',', ';', '\t'}
	best := ','
	bestCount := 0
	for _, candidate := range candidates {
		count := 0
		quoted := false
		for index := 0; index < len(line); index++ {
			if line[index] == '"' {
				if quoted && index+1 < len(line) && line[index+1] == '"' {
					index++
					continue
				}
				quoted = !quoted
				continue
			}
			if !quoted && rune(line[index]) == candidate {
				count++
			}
		}
		if count > bestCount {
			best = candidate
			bestCount = count
		}
	}
	return best
}

func finalizeTable(rows [][]string, rowNumbers []int, totalRows int, totalRowsExact bool) Preview {
	preview := Preview{
		Kind: "table", Rows: rows, RowNumbers: rowNumbers,
		TotalRows: totalRows, TotalRowsExact: totalRowsExact,
	}
	if len(rows) > MaxRows {
		preview.Rows = rows[:MaxRows]
		preview.RowNumbers = rowNumbers[:MaxRows]
		preview.TruncatedRows = true
	}
	columnCount := 0
	for _, row := range rows {
		if len(row) > columnCount {
			columnCount = len(row)
		}
	}
	if columnCount == 0 {
		columnCount = 1
	}
	if likelyHeader(rows) {
		preview.Columns = uniqueHeaders(rows[0], columnCount)
		preview.Rows = rows[1:]
		preview.RowNumbers = rowNumbers[1:]
		if preview.TotalRows > 0 {
			preview.TotalRows--
		}
		if len(preview.Rows) > MaxRows {
			preview.Rows = preview.Rows[:MaxRows]
			preview.RowNumbers = preview.RowNumbers[:MaxRows]
			preview.TruncatedRows = true
		}
	} else {
		preview.Columns = make([]string, columnCount)
		for index := range preview.Columns {
			preview.Columns[index] = columnName(index)
		}
	}
	for index, row := range preview.Rows {
		if len(row) < columnCount {
			preview.Rows[index] = append(row, make([]string, columnCount-len(row))...)
		}
	}
	preview.TruncatedRows = preview.TruncatedRows || !preview.TotalRowsExact || preview.TotalRows > len(preview.Rows)
	return preview
}

func likelyHeader(rows [][]string) bool {
	if len(rows) < 2 || len(rows[0]) < 2 {
		return false
	}
	seen := make(map[string]struct{}, len(rows[0]))
	nonempty := 0
	textual := 0
	headerHints := 0
	for _, raw := range rows[0] {
		value := strings.TrimSpace(raw)
		if value == "" {
			continue
		}
		nonempty++
		key := strings.ToLower(value)
		if _, duplicate := seen[key]; duplicate {
			return false
		}
		seen[key] = struct{}{}
		if _, err := strconv.ParseFloat(strings.ReplaceAll(value, ",", ""), 64); err != nil {
			textual++
		}
		if headerLabelHint(value) {
			headerHints++
		}
	}
	if nonempty < 2 || nonempty*2 < len(rows[0]) || textual*2 < nonempty {
		return false
	}
	if headerHints > 0 {
		return true
	}

	shapeDifferences := 0
	for column, raw := range rows[0] {
		first := strings.TrimSpace(raw)
		if first == "" {
			continue
		}
		firstShape := tableCellShape(first)
		samples := 0
		different := 0
		for row := 1; row < len(rows) && row <= 8; row++ {
			if column >= len(rows[row]) {
				continue
			}
			value := strings.TrimSpace(rows[row][column])
			if value == "" {
				continue
			}
			samples++
			if tableCellShape(value) != firstShape {
				different++
			}
		}
		if samples > 0 && different*2 >= samples {
			shapeDifferences++
		}
	}
	return shapeDifferences*2 >= nonempty
}

func headerLabelHint(value string) bool {
	value = strings.ToLower(strings.TrimSpace(value))
	value = strings.NewReplacer("_", " ", "-", " ", ".", " ").Replace(value)
	value = strings.Join(strings.Fields(value), " ")
	_, ok := map[string]struct{}{
		"id": {}, "no": {}, "number": {}, "name": {}, "company": {}, "ticker": {},
		"date": {}, "time": {}, "status": {}, "type": {}, "value": {}, "count": {},
		"total": {}, "description": {}, "note": {}, "tracking number": {}, "nomor": {},
		"nomor resi": {}, "resi": {}, "tanggal": {}, "nama": {}, "jumlah": {}, "keterangan": {},
	}[value]
	return ok
}

func tableCellShape(value string) string {
	if _, err := strconv.ParseFloat(strings.ReplaceAll(value, ",", ""), 64); err == nil {
		return "number"
	}
	hasLetter := false
	hasUpper := false
	hasLower := false
	hasOther := false
	firstLetterSet := false
	firstLetterUpper := false
	for _, character := range value {
		switch {
		case character >= 'A' && character <= 'Z':
			hasLetter, hasUpper = true, true
			if !firstLetterSet {
				firstLetterSet, firstLetterUpper = true, true
			}
		case character >= 'a' && character <= 'z':
			hasLetter, hasLower = true, true
			if !firstLetterSet {
				firstLetterSet = true
			}
		case character >= '0' && character <= '9', character == ' ', character == '_', character == '-':
		default:
			hasOther = true
		}
	}
	switch {
	case !hasLetter:
		return "symbol"
	case hasOther:
		return "decorated"
	case hasUpper && !hasLower:
		return "upper"
	case hasLower && !hasUpper:
		return "lower"
	case firstLetterUpper:
		return "title"
	default:
		return "mixed"
	}
}

func uniqueHeaders(row []string, count int) []string {
	headers := make([]string, count)
	seen := make(map[string]int, count)
	for index := 0; index < count; index++ {
		value := ""
		if index < len(row) {
			value = strings.TrimSpace(row[index])
		}
		if value == "" {
			value = columnName(index)
		}
		key := strings.ToLower(value)
		seen[key]++
		if seen[key] > 1 {
			value = fmt.Sprintf("%s (%d)", value, seen[key])
		}
		headers[index] = value
	}
	return headers
}

func boundRow(row []string) ([]string, bool, bool) {
	truncatedColumns := len(row) > MaxColumns
	if len(row) > MaxColumns {
		row = row[:MaxColumns]
	}
	result := make([]string, len(row))
	truncatedCells := false
	for index, value := range row {
		result[index], truncatedCells = boundCellAccumulating(value, truncatedCells)
	}
	return result, truncatedColumns, truncatedCells
}

func boundCellAccumulating(value string, alreadyTruncated bool) (string, bool) {
	bounded, truncated := boundCell(value)
	return bounded, alreadyTruncated || truncated
}

func boundCell(value string) (string, bool) {
	value = strings.ReplaceAll(value, "\x00", "")
	runes := []rune(value)
	if len(runes) <= MaxCellRunes {
		return value, false
	}
	return string(runes[:MaxCellRunes]) + "…", true
}

func columnName(index int) string {
	index++
	name := ""
	for index > 0 {
		index--
		name = string(rune('A'+index%26)) + name
		index /= 26
	}
	return name
}

type workbookDocument struct {
	Sheets []workbookSheet `xml:"sheets>sheet"`
}

type workbookSheet struct {
	Name           string `xml:"name,attr"`
	RelationshipID string `xml:"id,attr"`
}

type relationshipsDocument struct {
	Relationships []workbookRelationship `xml:"Relationship"`
}

type workbookRelationship struct {
	ID         string `xml:"Id,attr"`
	Target     string `xml:"Target,attr"`
	TargetMode string `xml:"TargetMode,attr"`
}

type richText struct {
	Value string
}

func (value *richText) UnmarshalXML(decoder *xml.Decoder, start xml.StartElement) error {
	var builder strings.Builder
	for {
		token, err := decoder.Token()
		if err != nil {
			return err
		}
		switch typed := token.(type) {
		case xml.StartElement:
			if typed.Name.Local == "t" {
				var text string
				if err := decoder.DecodeElement(&text, &typed); err != nil {
					return err
				}
				builder.WriteString(text)
			}
		case xml.EndElement:
			if typed.Name == start.Name {
				value.Value = builder.String()
				return nil
			}
		}
	}
}

type worksheetCell struct {
	Reference string   `xml:"r,attr"`
	Type      string   `xml:"t,attr"`
	Value     string   `xml:"v"`
	Inline    richText `xml:"is"`
}

type positionedCell struct {
	Column int
	Cell   worksheetCell
}

type worksheetRow struct {
	Number           int
	Cells            []positionedCell
	ColumnsTruncated bool
}

func (row *worksheetRow) UnmarshalXML(decoder *xml.Decoder, start xml.StartElement) error {
	for _, attribute := range start.Attr {
		if attribute.Name.Local == "r" {
			row.Number, _ = strconv.Atoi(attribute.Value)
		}
	}
	nextColumn := 0
	for {
		token, err := decoder.Token()
		if err != nil {
			return err
		}
		switch typed := token.(type) {
		case xml.StartElement:
			if typed.Name.Local != "c" {
				continue
			}
			var cell worksheetCell
			if err := decoder.DecodeElement(&cell, &typed); err != nil {
				return err
			}
			column, valid := cellColumn(cell.Reference)
			if !valid {
				column = nextColumn
			}
			nextColumn = column + 1
			if column >= MaxColumns {
				row.ColumnsTruncated = true
				continue
			}
			row.Cells = append(row.Cells, positionedCell{Column: column, Cell: cell})
		case xml.EndElement:
			if typed.Name == start.Name {
				return nil
			}
		}
	}
}

func buildXLSX(source io.ReaderAt, size int64, requestedSheet string) (Preview, error) {
	archive, err := zip.NewReader(source, size)
	if err != nil {
		return Preview{}, fmt.Errorf("%w: open XLSX: %v", ErrInvalid, err)
	}
	if len(archive.File) > maximumZipEntries {
		return Preview{}, fmt.Errorf("%w: XLSX contains too many entries", ErrInvalid)
	}
	files := make(map[string]*zip.File, len(archive.File))
	var expanded uint64
	for _, file := range archive.File {
		name := path.Clean(strings.TrimPrefix(file.Name, "/"))
		if name == "." || strings.HasPrefix(name, "../") {
			return Preview{}, fmt.Errorf("%w: XLSX contains an unsafe path", ErrInvalid)
		}
		if _, duplicate := files[name]; duplicate {
			return Preview{}, fmt.Errorf("%w: XLSX contains duplicate entries", ErrInvalid)
		}
		if file.UncompressedSize64 > maximumZipBytes || expanded > maximumZipBytes-file.UncompressedSize64 {
			return Preview{}, fmt.Errorf("%w: XLSX expands beyond the preview limit", ErrInvalid)
		}
		expanded += file.UncompressedSize64
		files[name] = file
	}
	workbookData, err := readZipFile(files["xl/workbook.xml"], maximumXMLBytes)
	if err != nil {
		return Preview{}, fmt.Errorf("%w: read XLSX workbook: %v", ErrInvalid, err)
	}
	relationshipData, err := readZipFile(files["xl/_rels/workbook.xml.rels"], maximumXMLBytes)
	if err != nil {
		return Preview{}, fmt.Errorf("%w: read XLSX relationships: %v", ErrInvalid, err)
	}
	var workbook workbookDocument
	if err := decodeXML(workbookData, &workbook); err != nil || len(workbook.Sheets) == 0 {
		return Preview{}, fmt.Errorf("%w: parse XLSX workbook", ErrInvalid)
	}
	var relationships relationshipsDocument
	if err := decodeXML(relationshipData, &relationships); err != nil {
		return Preview{}, fmt.Errorf("%w: parse XLSX relationships", ErrInvalid)
	}
	targets := make(map[string]string, len(relationships.Relationships))
	for _, relationship := range relationships.Relationships {
		if strings.EqualFold(relationship.TargetMode, "External") {
			continue
		}
		target := path.Clean(path.Join("xl", strings.TrimPrefix(relationship.Target, "/xl/")))
		if !strings.HasPrefix(target, "xl/worksheets/") {
			continue
		}
		targets[relationship.ID] = target
	}
	sheets := make([]string, 0, len(workbook.Sheets))
	sheetNames := make(map[string]struct{}, len(workbook.Sheets))
	selected := workbook.Sheets[0]
	found := requestedSheet == ""
	for _, current := range workbook.Sheets {
		if current.Name == "" {
			return Preview{}, fmt.Errorf("%w: XLSX contains an unnamed sheet", ErrInvalid)
		}
		if _, duplicate := sheetNames[current.Name]; duplicate {
			return Preview{}, fmt.Errorf("%w: XLSX contains duplicate sheet names", ErrInvalid)
		}
		sheetNames[current.Name] = struct{}{}
		sheets = append(sheets, current.Name)
		if requestedSheet != "" && current.Name == requestedSheet {
			selected = current
			found = true
		}
	}
	if !found {
		return Preview{}, ErrSheetMissing
	}
	target := targets[selected.RelationshipID]
	worksheetFile := files[target]
	if target == "" || worksheetFile == nil {
		return Preview{}, fmt.Errorf("%w: selected XLSX sheet is unavailable", ErrInvalid)
	}
	sharedStrings, err := parseSharedStrings(files["xl/sharedStrings.xml"])
	if err != nil {
		return Preview{}, fmt.Errorf("%w: parse XLSX shared strings: %v", ErrInvalid, err)
	}
	rows, rowNumbers, totalRows, truncatedRows, truncatedColumns, truncatedCells, err := parseWorksheet(worksheetFile, sharedStrings)
	if err != nil {
		return Preview{}, fmt.Errorf("%w: parse XLSX sheet: %v", ErrInvalid, err)
	}
	preview := finalizeTable(rows, rowNumbers, totalRows, true)
	preview.Sheets = sheets
	preview.Sheet = selected.Name
	preview.TruncatedRows = preview.TruncatedRows || truncatedRows
	preview.TruncatedColumns = truncatedColumns
	preview.TruncatedCells = truncatedCells
	return preview, nil
}

func decodeXML(data []byte, target any) error {
	decoder := xml.NewDecoder(bytes.NewReader(data))
	decoder.Strict = true
	return decoder.Decode(target)
}

func readZipFile(file *zip.File, maximum int64) ([]byte, error) {
	if file == nil {
		return nil, errors.New("required entry is missing")
	}
	if file.UncompressedSize64 > uint64(maximum) {
		return nil, errors.New("entry exceeds the preview limit")
	}
	reader, err := file.Open()
	if err != nil {
		return nil, err
	}
	defer reader.Close()
	data, err := io.ReadAll(io.LimitReader(reader, maximum+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > maximum {
		return nil, errors.New("entry exceeds the preview limit")
	}
	return data, nil
}

func parseSharedStrings(file *zip.File) ([]string, error) {
	if file == nil {
		return nil, nil
	}
	if file.UncompressedSize64 > maximumXMLBytes {
		return nil, errors.New("shared strings exceed the preview limit")
	}
	reader, err := file.Open()
	if err != nil {
		return nil, err
	}
	defer reader.Close()
	decoder := xml.NewDecoder(io.LimitReader(reader, maximumXMLBytes+1))
	decoder.Strict = true
	values := make([]string, 0)
	for {
		token, err := decoder.Token()
		if errors.Is(err, io.EOF) {
			return values, nil
		}
		if err != nil {
			return nil, err
		}
		start, ok := token.(xml.StartElement)
		if !ok || start.Name.Local != "si" {
			continue
		}
		var value richText
		if err := decoder.DecodeElement(&value, &start); err != nil {
			return nil, err
		}
		if len(values) >= maximumSharedItems {
			return nil, errors.New("shared strings contain too many entries")
		}
		values = append(values, value.Value)
	}
}

func parseWorksheet(file *zip.File, sharedStrings []string) ([][]string, []int, int, bool, bool, bool, error) {
	if file.UncompressedSize64 > maximumXMLBytes {
		return nil, nil, 0, false, false, false, errors.New("worksheet exceeds the preview limit")
	}
	reader, err := file.Open()
	if err != nil {
		return nil, nil, 0, false, false, false, err
	}
	defer reader.Close()
	decoder := xml.NewDecoder(io.LimitReader(reader, maximumXMLBytes+1))
	decoder.Strict = true
	rows := make([][]string, 0, MaxRows+2)
	rowNumbers := make([]int, 0, MaxRows+2)
	truncatedRows := false
	truncatedColumns := false
	truncatedCells := false
	totalRows := 0
	nextRow := 1
	for {
		token, err := decoder.Token()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, nil, 0, false, false, false, err
		}
		start, ok := token.(xml.StartElement)
		if !ok || start.Name.Local != "row" {
			continue
		}
		totalRows++
		if len(rows) >= MaxRows+2 {
			truncatedRows = true
			if err := decoder.Skip(); err != nil {
				return nil, nil, 0, false, false, false, err
			}
			continue
		}
		var row worksheetRow
		if err := decoder.DecodeElement(&row, &start); err != nil {
			return nil, nil, 0, false, false, false, err
		}
		if row.Number < 1 {
			row.Number = nextRow
		}
		nextRow = row.Number + 1
		maximumColumn := -1
		for _, positioned := range row.Cells {
			if positioned.Column > maximumColumn {
				maximumColumn = positioned.Column
			}
		}
		values := make([]string, maximumColumn+1)
		for _, positioned := range row.Cells {
			value, valueErr := worksheetCellValue(positioned.Cell, sharedStrings)
			if valueErr != nil {
				return nil, nil, 0, false, false, false, valueErr
			}
			values[positioned.Column], truncatedCells = boundCellAccumulating(value, truncatedCells)
		}
		rows = append(rows, values)
		rowNumbers = append(rowNumbers, row.Number)
		truncatedColumns = truncatedColumns || row.ColumnsTruncated
	}
	return rows, rowNumbers, totalRows, truncatedRows, truncatedColumns, truncatedCells, nil
}

func worksheetCellValue(cell worksheetCell, sharedStrings []string) (string, error) {
	switch cell.Type {
	case "s":
		index, err := strconv.Atoi(strings.TrimSpace(cell.Value))
		if err != nil || index < 0 || index >= len(sharedStrings) {
			return "", errors.New("shared string reference is invalid")
		}
		return sharedStrings[index], nil
	case "inlineStr":
		return cell.Inline.Value, nil
	case "b":
		if strings.TrimSpace(cell.Value) == "1" {
			return "TRUE", nil
		}
		return "FALSE", nil
	default:
		return cell.Value, nil
	}
}

func cellColumn(reference string) (int, bool) {
	column := 0
	letters := 0
	for _, character := range reference {
		if character >= 'a' && character <= 'z' {
			character -= 'a' - 'A'
		}
		if character < 'A' || character > 'Z' {
			break
		}
		column = column*26 + int(character-'A'+1)
		letters++
	}
	if letters == 0 {
		return 0, false
	}
	return column - 1, true
}
