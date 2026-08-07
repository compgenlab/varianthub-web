package catalog

// AssetPrefix is where assets live under a storage location.
//
// Shared across sources rather than nested under each, because the name is a
// content digest: the same conversion script pinned by two versions of a source
// is one object, and a re-registration that changes nothing writes nothing.
import "context"

const AssetPrefix = "assets/sha256"

// AssetStorage picks where assets live, preferring object storage.
//
// Not simply the default location. Every worker materializing a job has to read
// every asset its sources declare, so assets have to sit somewhere all of them
// can reach. A path location may be a shared mount or may be local to one pod,
// and nothing in the catalog distinguishes the two — so a path default would
// work on a single-node deployment and fail on the second replica, which is the
// failure this whole move to object storage exists to avoid.
//
// A bucket is reachable from every replica by construction, so when one is
// configured it wins. With no bucket at all the default location is the only
// answer available, and a single-node deployment is exactly where that is fine.
func AssetStorage(locs []StorageLocation) (StorageLocation, bool) {
	var fallback StorageLocation
	var haveFallback bool
	for _, l := range locs {
		if !l.Usable() {
			continue
		}
		if l.Kind == StorageS3 {
			// Among buckets, honour the deployment's own choice.
			if l.IsDefault {
				return l, true
			}
			if fallback.Kind != StorageS3 {
				fallback, haveFallback = l, true
			}
			continue
		}
		if !haveFallback || (l.IsDefault && fallback.Kind != StorageS3) {
			fallback, haveFallback = l, true
		}
	}
	return fallback, haveFallback
}

// AssetFiles lists the objects a source's helper files occupy, derived from the
// asset rows rather than recorded alongside downloads.
//
// Derived on purpose. source_file describes what a download produced and is
// replaced wholesale per (source, location), so recording assets there would
// have a download and a re-registration deleting each other's rows. These
// entries cannot go stale, because they are read from the same rows that decide
// what a job materializes.
//
// An asset still held inline is skipped: there is no object to report.
func (s *Store) AssetFiles(ctx context.Context, sourceID, storageID string) ([]SourceFile, error) {
	locs, err := s.ListStorage(ctx)
	if err != nil {
		return nil, err
	}
	loc, ok := AssetStorage(locs)
	if !ok || (storageID != "" && storageID != loc.ID) {
		return nil, nil
	}

	q := `SELECT source_id, name, sha256, size_bytes, created_at
	        FROM source_asset WHERE sha256 <> '' AND content IS NULL`
	args := []any{}
	if sourceID != "" {
		args = append(args, sourceID)
		q += " AND source_id=$1"
	}
	q += " ORDER BY source_id, name"

	rows, err := s.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	// Two sources sharing a script share one object, so report it once. Which
	// source it is attributed to is arbitrary and ordering makes it stable.
	seen := map[string]bool{}
	out := []SourceFile{}
	for rows.Next() {
		var f SourceFile
		var name, digest string
		if err := rows.Scan(&f.SourceID, &name, &digest, &f.SizeBytes, &f.ModifiedAt); err != nil {
			return nil, err
		}
		if seen[digest] {
			continue
		}
		seen[digest] = true
		f.StorageID = loc.ID
		f.Path = AssetPrefix + "/" + digest
		f.RecordedAt = f.ModifiedAt
		out = append(out, f)
	}
	return out, rows.Err()
}
