package queries

import (
	"context"
	databasesql "database/sql"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/jmoiron/sqlx"
	"github.com/marcboeker/go-duckdb"
	runtimev1 "github.com/rilldata/rill/proto/gen/rill/runtime/v1"
	"github.com/rilldata/rill/runtime"
	"github.com/rilldata/rill/runtime/drivers"
	duckdbolap "github.com/rilldata/rill/runtime/drivers/duckdb"
	"github.com/rilldata/rill/runtime/pkg/pbutil"
	"google.golang.org/protobuf/types/known/structpb"
)

type MetricsViewAggregation struct {
	MetricsViewName    string                                       `json:"metrics_view,omitempty"`
	Dimensions         []*runtimev1.MetricsViewAggregationDimension `json:"dimensions,omitempty"`
	Measures           []*runtimev1.MetricsViewAggregationMeasure   `json:"measures,omitempty"`
	Sort               []*runtimev1.MetricsViewAggregationSort      `json:"sort,omitempty"`
	TimeRange          *runtimev1.TimeRange                         `json:"time_range,omitempty"`
	Where              *runtimev1.Expression                        `json:"where,omitempty"`
	Having             *runtimev1.Expression                        `json:"having,omitempty"`
	Priority           int32                                        `json:"priority,omitempty"`
	Limit              *int64                                       `json:"limit,omitempty"`
	Offset             int64                                        `json:"offset,omitempty"`
	MetricsView        *runtimev1.MetricsViewSpec                   `json:"-"`
	ResolvedMVSecurity *runtime.ResolvedMetricsViewSecurity         `json:"security"`
	PivotOn            []string                                     `json:"pivot_on,omitempty"`

	// backwards compatibility
	Filter *runtimev1.MetricsViewFilter `json:"filter,omitempty"`

	Result *runtimev1.MetricsViewAggregationResponse `json:"-"`
}

var maxPivotCells = 1_000_000

var _ runtime.Query = &MetricsViewAggregation{}

func (q *MetricsViewAggregation) Key() string {
	r, err := json.Marshal(q)
	if err != nil {
		panic(err)
	}
	return fmt.Sprintf("MetricsViewAggregation:%s", string(r))
}

func (q *MetricsViewAggregation) Deps() []*runtimev1.ResourceName {
	return []*runtimev1.ResourceName{
		{Kind: runtime.ResourceKindMetricsView, Name: q.MetricsViewName},
	}
}

func (q *MetricsViewAggregation) MarshalResult() *runtime.QueryResult {
	return &runtime.QueryResult{
		Value: q.Result,
		Bytes: sizeProtoMessage(q.Result),
	}
}

func (q *MetricsViewAggregation) UnmarshalResult(v any) error {
	res, ok := v.(*runtimev1.MetricsViewAggregationResponse)
	if !ok {
		return fmt.Errorf("MetricsViewAggregation: mismatched unmarshal input")
	}
	q.Result = res
	return nil
}

func (q *MetricsViewAggregation) Resolve(ctx context.Context, rt *runtime.Runtime, instanceID string, priority int) error {
	olap, release, err := rt.OLAP(ctx, instanceID)
	if err != nil {
		return err
	}
	defer release()

	if olap.Dialect() != drivers.DialectDuckDB && olap.Dialect() != drivers.DialectDruid {
		return fmt.Errorf("not available for dialect '%s'", olap.Dialect())
	}

	if q.MetricsView.TimeDimension == "" && !isTimeRangeNil(q.TimeRange) {
		return fmt.Errorf("metrics view '%s' does not have a time dimension", q.MetricsView)
	}

	// backwards compatibility
	if q.Filter != nil {
		if q.Where != nil {
			return fmt.Errorf("both filter and where is provided")
		}
		q.Where = convertFilterToExpression(q.Filter)
	}

	// Build query
	sqlString, args, err := q.buildMetricsAggregationSQL(q.MetricsView, olap.Dialect(), q.ResolvedMVSecurity)
	if err != nil {
		return fmt.Errorf("error building query: %w", err)
	}

	if len(q.PivotOn) == 0 {
		schema, data, err := olapQuery(ctx, olap, priority, sqlString, args)
		if err != nil {
			return err
		}

		q.Result = &runtimev1.MetricsViewAggregationResponse{
			Schema: schema,
			Data:   data,
		}
		return nil
	}

	if olap.Dialect() == drivers.DialectDuckDB {
		return olap.WithConnection(ctx, priority, false, false, func(ctx context.Context, ensuredCtx context.Context, conn *databasesql.Conn) error {
			temporaryTableName := tempName("_for_pivot_")

			err := olap.Exec(ctx, &drivers.Statement{
				Query:    fmt.Sprintf("CREATE TEMPORARY TABLE %[1]s AS %[2]s", temporaryTableName, sqlString),
				Args:     args,
				Priority: priority,
			})
			if err != nil {
				return err
			}

			res, err := olap.Execute(ctx, &drivers.Statement{ // a separate query instead of the multi-statement query due to a DuckDB bug
				Query:    fmt.Sprintf("SELECT COUNT(*) FROM %[1]s", temporaryTableName),
				Priority: priority,
			})
			if err != nil {
				return err
			}

			count := 0
			if res.Next() {
				err := res.Scan(&count)
				if err != nil {
					res.Close()
					return err
				}

				if count > maxPivotCells/q.cols() {
					res.Close()
					return fmt.Errorf("PIVOT cells count exceeded %d", maxPivotCells)
				}
			}
			res.Close()

			defer func() {
				_ = olap.Exec(ensuredCtx, &drivers.Statement{
					Query: `DROP TABLE "` + temporaryTableName + `"`,
				})
			}()

			schema, data, err := olapQuery(ctx, olap, int(q.Priority), q.createPivotSQL(temporaryTableName), nil)
			if err != nil {
				return err
			}

			q.Result = &runtimev1.MetricsViewAggregationResponse{
				Schema: schema,
				Data:   data,
			}

			return nil
		})
	}

	rows, err := olap.Execute(ctx, &drivers.Statement{
		Query:            sqlString,
		Args:             args,
		Priority:         priority,
		ExecutionTimeout: defaultExecutionTimeout,
	})
	if err != nil {
		return nil
	}
	defer rows.Close()

	return q.pivotDruid(ctx, rows)
}

func (q *MetricsViewAggregation) pivotDruid(ctx context.Context, rows *drivers.Result) error {
	pivotDB, err := sqlx.Connect("duckdb", "")
	if err != nil {
		return err
	}
	defer pivotDB.Close()

	return func() error {
		temporaryTableName := tempName("_for_pivot_")
		createTableSQL, err := duckdbolap.CreateTableQuery(rows.Schema, temporaryTableName)
		if err != nil {
			return err
		}

		_, err = pivotDB.ExecContext(ctx, createTableSQL)
		if err != nil {
			return err
		}
		defer func() {
			_, _ = pivotDB.ExecContext(context.Background(), `DROP TABLE "`+temporaryTableName+`"`)
		}()

		conn, err := pivotDB.Conn(ctx)
		if err != nil {
			return nil
		}
		defer conn.Close()

		err = conn.Raw(func(conn any) error {
			driverCon, ok := conn.(driver.Conn)
			if !ok {
				return fmt.Errorf("cannot obtain driver.Conn")
			}
			appender, err := duckdb.NewAppenderFromConn(driverCon, "", temporaryTableName)
			if err != nil {
				return err
			}
			defer appender.Close()

			batchSize := 10000
			columns, err := rows.Columns()
			if err != nil {
				return err
			}

			scanValues := make([]any, len(columns))
			appendValues := make([]driver.Value, len(columns))
			for i := range scanValues {
				scanValues[i] = new(interface{})
			}
			count := 0
			maxCount := maxPivotCells / q.cols()

			for rows.Next() {
				err = rows.Scan(scanValues...)
				if err != nil {
					return err
				}
				for i := range columns {
					appendValues[i] = driver.Value(*(scanValues[i].(*interface{})))
				}
				err = appender.AppendRowArray(appendValues)
				if err != nil {
					return err
				}
				count++
				if count > maxCount {
					return fmt.Errorf("PIVOT cells count limit exceeded %d", maxPivotCells)
				}

				if count >= batchSize {
					appender.Flush()
					count = 0
				}
			}
			appender.Flush()

			return nil
		})
		if err != nil {
			return err
		}
		if rows.Err() != nil {
			return rows.Err()
		}

		ctx, cancelFunc := context.WithTimeout(ctx, defaultExecutionTimeout)
		defer cancelFunc()
		pivotRows, err := pivotDB.QueryxContext(ctx, q.createPivotSQL(temporaryTableName))
		if err != nil {
			return err
		}
		defer pivotRows.Close()

		schema, err := duckdbolap.RowsToSchema(pivotRows)
		if err != nil {
			return err
		}

		data, err := toData(pivotRows, schema)
		if err != nil {
			return err
		}

		q.Result = &runtimev1.MetricsViewAggregationResponse{
			Schema: schema,
			Data:   data,
		}

		return nil
	}()
}

func (q *MetricsViewAggregation) createPivotSQL(temporaryTableName string) string {
	measureCols := make([]string, 0, len(q.Measures))
	for _, m := range q.Measures {
		sn := safeName(m.Name)
		measureCols = append(measureCols, fmt.Sprintf("LAST(%s) as %s", sn, sn))
	}

	sortingCriteria := make([]string, 0, len(q.Sort))
	for _, s := range q.Sort {
		sortCriterion := safeName(s.Name)
		if s.Desc {
			sortCriterion += " DESC"
		}
		sortCriterion += " NULLS LAST"
		sortingCriteria = append(sortingCriteria, sortCriterion)
	}

	orderClause := ""
	if len(sortingCriteria) > 0 {
		orderClause = "ORDER BY " + strings.Join(sortingCriteria, ", ")
	}

	var limitClause string
	if q.Limit != nil {
		if *q.Limit == 0 {
			*q.Limit = 100
		}
		limitClause = fmt.Sprintf("LIMIT %d", *q.Limit)
	}

	//	PIVOT t ON year USING LAST(ap) ap;
	return fmt.Sprintf("PIVOT %[1]s ON %[2]s USING %[3]s %[4]s %[5]s OFFSET %[6]d",
		temporaryTableName,              // 1
		strings.Join(q.PivotOn, ", "),   // 2
		strings.Join(measureCols, ", "), // 3
		orderClause,                     // 4
		limitClause,                     // 5
		q.Offset,                        // 6
	)
}

func toData(rows *sqlx.Rows, schema *runtimev1.StructType) ([]*structpb.Struct, error) {
	var data []*structpb.Struct
	for rows.Next() {
		rowMap := make(map[string]any)
		err := rows.MapScan(rowMap)
		if err != nil {
			return nil, err
		}

		rowStruct, err := pbutil.ToStruct(rowMap, schema)
		if err != nil {
			return nil, err
		}

		data = append(data, rowStruct)
	}

	err := rows.Err()
	if err != nil {
		return nil, err
	}

	return data, nil
}

func (q *MetricsViewAggregation) Export(ctx context.Context, rt *runtime.Runtime, instanceID string, w io.Writer, opts *runtime.ExportOptions) error {
	err := q.Resolve(ctx, rt, instanceID, opts.Priority)
	if err != nil {
		return err
	}

	filename := strings.ReplaceAll(q.MetricsView.Table, `"`, `_`)
	if !isTimeRangeNil(q.TimeRange) || q.Where != nil || q.Having != nil {
		filename += "_filtered"
	}

	meta := structTypeToMetricsViewColumn(q.Result.Schema)

	if opts.PreWriteHook != nil {
		err = opts.PreWriteHook(filename)
		if err != nil {
			return err
		}
	}

	switch opts.Format {
	case runtimev1.ExportFormat_EXPORT_FORMAT_UNSPECIFIED:
		return fmt.Errorf("unspecified format")
	case runtimev1.ExportFormat_EXPORT_FORMAT_CSV:
		return writeCSV(meta, q.Result.Data, w)
	case runtimev1.ExportFormat_EXPORT_FORMAT_XLSX:
		return writeXLSX(meta, q.Result.Data, w)
	case runtimev1.ExportFormat_EXPORT_FORMAT_PARQUET:
		return writeParquet(meta, q.Result.Data, w)
	}

	return nil
}

func (q *MetricsViewAggregation) cols() int {
	return len(q.Dimensions) + len(q.Measures)
}

func (q *MetricsViewAggregation) buildMetricsAggregationSQL(mv *runtimev1.MetricsViewSpec, dialect drivers.Dialect, policy *runtime.ResolvedMetricsViewSecurity) (string, []any, error) {
	if len(q.Dimensions) == 0 && len(q.Measures) == 0 {
		return "", nil, errors.New("no dimensions or measures specified")
	}

	cols := q.cols()
	selectCols := make([]string, 0, cols)

	groupCols := make([]string, 0, len(q.Dimensions))
	unnestClauses := make([]string, 0)
	var args []any
	for _, d := range q.Dimensions {
		// Handle regular dimensions
		if d.TimeGrain == runtimev1.TimeGrain_TIME_GRAIN_UNSPECIFIED {
			dim, err := metricsViewDimension(mv, d.Name)
			if err != nil {
				return "", nil, err
			}
			dimSel, unnestClause := dimensionSelect(mv, dim, dialect)
			selectCols = append(selectCols, dimSel)
			if unnestClause != "" {
				unnestClauses = append(unnestClauses, unnestClause)
			}
			groupCols = append(groupCols, fmt.Sprintf("%d", len(selectCols)))
			continue
		}

		// Handle time dimension
		expr, exprArgs, err := q.buildTimestampExpr(d, dialect)
		if err != nil {
			return "", nil, err
		}
		selectCols = append(selectCols, fmt.Sprintf("%s as %s", expr, safeName(d.Name)))
		// Using expr was causing issues with query arg expansion in duckdb.
		// Using column name is not possible either since it will take the original column name instead of the aliased column name
		// But using numbered group we can exactly target the correct selected column.
		// Note that the non-timestamp columns also use the numbered group-by for constancy.
		groupCols = append(groupCols, fmt.Sprintf("%d", len(selectCols)))
		args = append(args, exprArgs...)
	}

	for _, m := range q.Measures {
		sn := safeName(m.Name)
		switch m.BuiltinMeasure {
		case runtimev1.BuiltinMeasure_BUILTIN_MEASURE_UNSPECIFIED:
			expr, err := metricsViewMeasureExpression(mv, m.Name)
			if err != nil {
				return "", nil, err
			}
			selectCols = append(selectCols, fmt.Sprintf("%s as %s", expr, sn))
		case runtimev1.BuiltinMeasure_BUILTIN_MEASURE_COUNT:
			selectCols = append(selectCols, fmt.Sprintf("COUNT(*) as %s", sn))
		case runtimev1.BuiltinMeasure_BUILTIN_MEASURE_COUNT_DISTINCT:
			if len(m.BuiltinMeasureArgs) != 1 {
				return "", nil, fmt.Errorf("builtin measure '%s' expects 1 argument", m.BuiltinMeasure.String())
			}
			arg := m.BuiltinMeasureArgs[0].GetStringValue()
			if arg == "" {
				return "", nil, fmt.Errorf("builtin measure '%s' expects non-empty string argument, got '%v'", m.BuiltinMeasure.String(), m.BuiltinMeasureArgs[0])
			}
			selectCols = append(selectCols, fmt.Sprintf("COUNT(DISTINCT %s) as %s", safeName(arg), sn))
		default:
			return "", nil, fmt.Errorf("unknown builtin measure '%d'", m.BuiltinMeasure)
		}
	}

	groupClause := ""
	if len(groupCols) > 0 {
		groupClause = "GROUP BY " + strings.Join(groupCols, ", ")
	}

	whereClause := ""
	if mv.TimeDimension != "" {
		timeCol := safeName(mv.TimeDimension)
		clause, err := timeRangeClause(q.TimeRange, mv, dialect, timeCol, &args)
		if err != nil {
			return "", nil, err
		}
		whereClause += clause
	}
	if q.Where != nil {
		clause, clauseArgs, err := buildExpression(mv, q.Where, nil, dialect)
		if err != nil {
			return "", nil, err
		}
		if strings.TrimSpace(clause) != "" {
			whereClause += " AND " + clause
		}
		args = append(args, clauseArgs...)
	}
	if policy != nil && policy.RowFilter != "" {
		whereClause += fmt.Sprintf(" AND (%s)", policy.RowFilter)
	}
	if len(whereClause) > 0 {
		whereClause = "WHERE 1=1" + whereClause
	}

	havingClause := ""
	if q.Having != nil {
		var havingClauseArgs []any
		var err error
		havingClause, havingClauseArgs, err = buildExpression(mv, q.Having, nil, dialect)
		if err != nil {
			return "", nil, err
		}
		if strings.TrimSpace(havingClause) != "" {
			havingClause = "HAVING " + havingClause
		}
		args = append(args, havingClauseArgs...)
	}

	sortingCriteria := make([]string, 0, len(q.Sort))
	for _, s := range q.Sort {
		sortCriterion := safeName(s.Name)
		if s.Desc {
			sortCriterion += " DESC"
		}
		if dialect == drivers.DialectDuckDB {
			sortCriterion += " NULLS LAST"
		}
		sortingCriteria = append(sortingCriteria, sortCriterion)
	}
	orderClause := ""
	if len(sortingCriteria) > 0 {
		orderClause = "ORDER BY " + strings.Join(sortingCriteria, ", ")
	}

	var limitClause string
	if q.Limit != nil {
		if *q.Limit == 0 {
			*q.Limit = 100
		}
		limitClause = fmt.Sprintf("LIMIT %d", *q.Limit)
	}

	var sql string
	if len(q.PivotOn) > 0 {
		l := maxPivotCells / q.cols()
		limitClause = fmt.Sprintf("LIMIT %d", l+1)

		if q.Offset != 0 {
			return "", nil, fmt.Errorf("offset not supported for pivot queries")
		}

		// select m1, m2, d1, d2 from t, lateral unnest(t.d1) tbl(unnested_d1_) where d1 = 'a' group by d1, d2
		sql = fmt.Sprintf("SELECT %[1]s FROM %[2]s %[3]s %[4]s %[5]s %[6]s %[7]s %[8]s",
			strings.Join(selectCols, ", "),  // 1
			safeName(mv.Table),              // 2
			strings.Join(unnestClauses, ""), // 3
			whereClause,                     // 4
			groupClause,                     // 5
			havingClause,                    // 6
			orderClause,                     // 7
			limitClause,                     // 8
		)
	} else {
		sql = fmt.Sprintf("SELECT %s FROM %s %s %s %s %s %s %s OFFSET %d",
			strings.Join(selectCols, ", "),
			safeName(mv.Table),
			strings.Join(unnestClauses, ""),
			whereClause,
			groupClause,
			havingClause,
			orderClause,
			limitClause,
			q.Offset,
		)
	}

	return sql, args, nil
}

func (q *MetricsViewAggregation) buildTimestampExpr(dim *runtimev1.MetricsViewAggregationDimension, dialect drivers.Dialect) (string, []any, error) {
	var col string
	if dim.Name == q.MetricsView.TimeDimension {
		col = safeName(dim.Name)
	} else {
		d, err := metricsViewDimension(q.MetricsView, dim.Name)
		if err != nil {
			return "", nil, err
		}
		if d.Expression != "" {
			// TODO: we should add support for this in a future PR
			return "", nil, fmt.Errorf("expression dimension not supported as time column")
		}
		col = metricsViewDimensionExpression(d)
	}

	switch dialect {
	case drivers.DialectDuckDB:
		if dim.TimeZone == "" || dim.TimeZone == "UTC" {
			return fmt.Sprintf("date_trunc('%s', %s)", convertToDateTruncSpecifier(dim.TimeGrain), col), nil, nil
		}
		return fmt.Sprintf("timezone(?, date_trunc('%s', timezone(?, %s::TIMESTAMPTZ)))", convertToDateTruncSpecifier(dim.TimeGrain), col), []any{dim.TimeZone, dim.TimeZone}, nil
	case drivers.DialectDruid:
		if dim.TimeZone == "" || dim.TimeZone == "UTC" {
			return fmt.Sprintf("date_trunc('%s', %s)", convertToDateTruncSpecifier(dim.TimeGrain), col), nil, nil
		}
		return fmt.Sprintf("time_floor(%s, '%s', null, CAST(? AS VARCHAR)))", col, convertToDruidTimeFloorSpecifier(dim.TimeGrain)), []any{dim.TimeZone}, nil
	default:
		return "", nil, fmt.Errorf("unsupported dialect %q", dialect)
	}
}
