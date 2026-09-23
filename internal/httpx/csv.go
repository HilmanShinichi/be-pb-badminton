package httpx

import (
	"bytes"
	"encoding/csv"

	"github.com/gofiber/fiber/v2"
)

// CSV streams a report as a download; used by the reports page export.
func CSV(c *fiber.Ctx, filename string, header []string, rows [][]string) error {
	var buf bytes.Buffer
	cw := csv.NewWriter(&buf)
	_ = cw.Write(header)
	for _, row := range rows {
		_ = cw.Write(row)
	}
	cw.Flush()
	c.Set("Content-Type", "text/csv; charset=utf-8")
	c.Set("Content-Disposition", `attachment; filename="`+filename+`"`)
	return c.Send(buf.Bytes())
}
