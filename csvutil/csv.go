package csvutil

import (
	"encoding/csv"
	"fmt"
	"io"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/KevinZhao/SmartRenew/model"
)

var headers = []string{
	"account_alias", "account_id", "region", "type", "resource_id",
	"instance_type", "platform", "quantity", "start_time", "end_time",
	"status", "description",
}

func Export(w io.Writer, rows []model.Reservation) error {
	cw := csv.NewWriter(w)

	if err := cw.Write(headers); err != nil {
		return err
	}
	for _, r := range rows {
		record := []string{
			r.AccountAlias, r.AccountID, r.Region, string(r.Type), r.ResourceID,
			r.InstanceType, r.Platform, strconv.Itoa(r.Quantity),
			r.StartTime.Format(time.RFC3339), r.EndTime.Format(time.RFC3339),
			r.Status, r.Description,
		}
		if err := cw.Write(record); err != nil {
			return err
		}
	}
	cw.Flush()
	return cw.Error()
}

func Import(r io.Reader) ([]model.Reservation, error) {
	cr := csv.NewReader(r)
	records, err := cr.ReadAll()
	if err != nil {
		return nil, fmt.Errorf("read csv: %w", err)
	}
	if len(records) < 2 {
		return nil, nil
	}

	headerRow := records[0]
	colMap := make(map[string]int)
	for i, h := range headerRow {
		colMap[strings.TrimSpace(h)] = i
	}

	var results []model.Reservation
	for i, row := range records[1:] {
		r := model.Reservation{
			AccountAlias: getCol(row, colMap, "account_alias"),
			AccountID:    getCol(row, colMap, "account_id"),
			Region:       getCol(row, colMap, "region"),
			Type:         model.ResourceType(getCol(row, colMap, "type")),
			ResourceID:   getCol(row, colMap, "resource_id"),
			InstanceType: getCol(row, colMap, "instance_type"),
			Platform:     getCol(row, colMap, "platform"),
			Status:       getCol(row, colMap, "status"),
			Description:  getCol(row, colMap, "description"),
		}
		r.ID = fmt.Sprintf("%s_%s_%s", r.AccountID, r.Region, r.ResourceID)

		if q := getCol(row, colMap, "quantity"); q != "" {
			r.Quantity, _ = strconv.Atoi(q)
		} else {
			r.Quantity = 1
		}
		if s := getCol(row, colMap, "start_time"); s != "" {
			if t, err := time.Parse(time.RFC3339, s); err == nil {
				r.StartTime = t
			} else {
				slog.Warn("parse start_time in csv", "row", i+2, "value", s, "err", err)
			}
		}
		if s := getCol(row, colMap, "end_time"); s != "" {
			if t, err := time.Parse(time.RFC3339, s); err == nil {
				r.EndTime = t
			} else {
				slog.Warn("parse end_time in csv", "row", i+2, "value", s, "err", err)
			}
		}

		// Both fields required to form a valid record
		if r.ResourceID != "" && r.AccountID != "" {
			results = append(results, r)
		}
	}
	return results, nil
}

func getCol(row []string, colMap map[string]int, key string) string {
	if idx, ok := colMap[key]; ok && idx < len(row) {
		return strings.TrimSpace(row[idx])
	}
	return ""
}
