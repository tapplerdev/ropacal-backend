package handlers

import (
	"database/sql/driver"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/jmoiron/sqlx"

	"ropacal-backend/internal/orgdb"
)

// A read that fails while rows stream back (a dropped connection, a statement
// timeout) surfaces only in rows.Err(). Both lists used to answer 200 with
// whatever they had read by then; now they answer 500. The mock fails the
// first row read. It's a mock because lib/pq waits for the first row before
// the query call returns, so Postgres would have to fail after a row has gone
// out, and only the crash-log query (its LIMIT lets a costly column run after
// the sort) can be made to do that.
func TestListsFailWhenReadingRowsFails(t *testing.T) {
	for name, tc := range map[string]struct {
		handler http.HandlerFunc
		query   string
		columns int
	}{
		"crash log":  {GetAppErrorLogs(nil), `FROM app_error_logs el`, 24},
		"bin checks": {GetBinCheckRecommendations(nil), `FROM bin_check_recommendations bcr`, 21},
	} {
		t.Run(name, func(t *testing.T) {
			raw, mock, err := sqlmock.New()
			if err != nil {
				t.Fatal(err)
			}
			defer raw.Close()
			cols := make([]string, tc.columns)
			for i := range cols {
				cols[i] = fmt.Sprintf("c%d", i)
			}
			mock.ExpectQuery(tc.query).WillReturnRows(sqlmock.NewRows(cols).
				AddRow(make([]driver.Value, tc.columns)...).
				RowError(0, errors.New("connection reset by peer")))

			r := httptest.NewRequest(http.MethodGet, "/x", nil)
			r = r.WithContext(orgdb.NewContext(r.Context(), orgdb.Passthrough(sqlx.NewDb(raw, "sqlmock"))))
			w := httptest.NewRecorder()
			tc.handler.ServeHTTP(w, r)
			if w.Code != http.StatusInternalServerError {
				t.Fatalf("%d %s, want 500", w.Code, w.Body.String())
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Fatal(err)
			}
		})
	}
}
