package query

import "context"

// LabelKey identifies one labelled thing on a host: the kind is one of the
// store.Label constants and the name is what the host reports.
type LabelKey struct {
	Kind string
	Name string
}

// Labels is one host's operator-given names. A nil Labels is valid and
// labels nothing.
type Labels map[LabelKey]string

// Of returns the label for kind and name when one is set, and name itself
// otherwise, so a caller never has to ask whether one exists.
func (l Labels) Of(kind, name string) string {
	if v, ok := l[LabelKey{kind, name}]; ok {
		return v
	}
	return name
}

// Has reports whether kind and name carry a label.
func (l Labels) Has(kind, name string) bool {
	_, ok := l[LabelKey{kind, name}]
	return ok
}

// fleetLabels fills each host's Labels. One query for the whole list, not
// one per host; the table is small by nature, a few rows per host at most.
func (q *Q) fleetLabels(ctx context.Context, fleet []Overview) error {
	rows, err := q.db.QueryContext(ctx, `SELECT host_id, kind, name, label FROM labels`)
	if err != nil {
		return err
	}
	defer rows.Close()

	byID := make(map[int64]*Overview, len(fleet))
	for i := range fleet {
		byID[fleet[i].ID] = &fleet[i]
	}
	for rows.Next() {
		var id int64
		var k LabelKey
		var label string
		if err := rows.Scan(&id, &k.Kind, &k.Name, &label); err != nil {
			return err
		}
		o := byID[id]
		if o == nil {
			continue
		}
		if o.Labels == nil {
			o.Labels = Labels{}
		}
		o.Labels[k] = label
	}
	return rows.Err()
}
