package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strings"

	"cloud.google.com/go/firestore"
	"google.golang.org/api/iterator"
	"google.golang.org/api/option"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/miere/murtaugh/internal/config"
)

// firestoreStore implements config.Store over Google Firestore.
//
// It is a separate implementation rather than a third Dialect: the SQL store is
// written against database/sql and parameterized only by the handful of syntax
// differences between SQLite and Postgres, and Firestore shares none of that.
// What it does share is the shape of the data — the config store is already a
// document store (JSON bodies keyed by (section, name) or by singleton key),
// so the mapping is direct and no schema migration machinery is needed.
//
// Firestore is here because it is the backend a distributed deployment can
// actually reach. A Postgres store good enough for leader election would have
// to be reachable from every node, which for a workstation node means standing
// up a tunnel to production — infrastructure the deployment does not otherwise
// need. Firestore is reachable from anywhere ADC works and is strongly
// consistent, which is the property leader election requires.
type firestoreStore struct {
	client *firestore.Client
	root   string
}

// Firestore document field names. They mirror the SQL columns so a store dumped
// from one backend reads the same as the other.
const (
	fsFieldSection   = "section"
	fsFieldName      = "name"
	fsFieldKey       = "key"
	fsFieldBody      = "body"
	fsFieldUpdatedAt = "updated_at"
)

// firestoreBatchLimit is Firestore's cap on writes in one batch. Restore chunks
// at this size.
const firestoreBatchLimit = 500

// openFirestore connects to the Firestore config store described by fsc.
//
// Authentication is Application Default Credentials by default, so a node
// running on GKE, Cloud Run, or a Compute Engine VM authenticates off its
// attached service account with nothing configured. CredentialsFile overrides
// that with an explicit key file for the hosts ADC cannot serve. An empty
// ProjectID is passed through as firestore.DetectProjectID so the project comes
// from the same credentials rather than having to be restated.
//
// Unlike the SQL backends there is no schema to migrate: Firestore creates
// collections on first write, and the documents are self-describing.
func openFirestore(ctx context.Context, fsc config.FirestoreConfig) (config.Store, error) {
	client, err := newFirestoreClient(ctx, fsc)
	if err != nil {
		return nil, err
	}
	return &firestoreStore{client: client, root: fsc.EffectiveCollection()}, nil
}

// newFirestoreClient builds the Firestore client for fsc, resolving credentials
// and probing the connection so a misconfigured project or a missing grant
// fails at startup rather than on the first config read.
func newFirestoreClient(ctx context.Context, fsc config.FirestoreConfig) (*firestore.Client, error) {
	projectID := strings.TrimSpace(fsc.ProjectID)
	if projectID == "" {
		projectID = firestore.DetectProjectID
	}

	var opts []option.ClientOption
	if path := fsc.EffectiveCredentialsFile(); path != "" {
		opts = append(opts, option.WithCredentialsFile(path))
	}

	client, err := firestore.NewClientWithDatabase(ctx, projectID, fsc.EffectiveDatabaseID(), opts...)
	if err != nil {
		return nil, fmt.Errorf("open firestore (project %q, database %q): %w",
			fsc.ProjectID, fsc.EffectiveDatabaseID(), err)
	}
	return client, nil
}

func (s *firestoreStore) Backend() string { return config.BackendFirestore }

func (s *firestoreStore) Close() error { return s.client.Close() }

// items is the collection holding the (section, name) entities.
func (s *firestoreStore) items() *firestore.CollectionRef {
	return s.client.Collection(s.root + "_items")
}

// singletons is the collection holding the single-valued blocks.
func (s *firestoreStore) singletons() *firestore.CollectionRef {
	return s.client.Collection(s.root + "_singletons")
}

// itemDocID renders a (section, name) pair as a Firestore document ID.
//
// The name is escaped because Firestore forbids "/" in a document ID and config
// entity names are free-form. The section prefix does double duty: it keeps two
// sections' identically-named entities apart, and it guarantees the ID can
// never match Firestore's reserved __.*__ pattern, which an unprefixed name
// could.
func itemDocID(section, name string) string {
	return section + "~" + url.PathEscape(name)
}

// Load reads every row and delegates to config.AssembleFromRows, so a
// Firestore-sourced Config is validated by exactly the same core as a
// SQLite- or Postgres-sourced one.
func (s *firestoreStore) Load(ctx context.Context, base config.Config) (config.Config, error) {
	items := make(map[string]map[string]json.RawMessage, len(config.AllSections))
	for _, section := range config.AllSections {
		rows, err := s.ListItems(ctx, section)
		if err != nil {
			return config.Config{}, err
		}
		items[section] = rows
	}
	singletons := make(map[string]json.RawMessage, len(config.AllSingletons))
	for _, key := range config.AllSingletons {
		body, ok, err := s.GetSingleton(ctx, key)
		if err != nil {
			return config.Config{}, err
		}
		if ok {
			singletons[key] = body
		}
	}
	return config.AssembleFromRows(base, items, singletons)
}

func (s *firestoreStore) UpsertItem(ctx context.Context, section, name string, body any) error {
	if !config.ValidSection(section) {
		return fmt.Errorf("unknown config section %q", section)
	}
	if strings.TrimSpace(name) == "" {
		return errors.New("config item name must not be blank")
	}
	raw, err := marshalBody(body)
	if err != nil {
		return err
	}
	doc := s.items().Doc(itemDocID(section, name))
	if _, err := doc.Set(ctx, map[string]any{
		fsFieldSection: section,
		fsFieldName:    name,
		fsFieldBody:    string(raw),
		// The server's clock, never this node's: writes arrive from several
		// machines whose clocks need not agree.
		fsFieldUpdatedAt: firestore.ServerTimestamp,
	}); err != nil {
		return fmt.Errorf("upsert %s/%s: %w", section, name, err)
	}
	return nil
}

func (s *firestoreStore) DeleteItem(ctx context.Context, section, name string) (bool, error) {
	doc := s.items().Doc(itemDocID(section, name))
	// Firestore's Delete is a no-op on a missing document and reports no error,
	// but callers need to know whether a row existed, so read first. The two
	// steps race only against another admin editing the same entity at the same
	// moment, which the SQL backends do not serialize either.
	if _, err := doc.Get(ctx); err != nil {
		if status.Code(err) == codes.NotFound {
			return false, nil
		}
		return false, fmt.Errorf("delete %s/%s: %w", section, name, err)
	}
	if _, err := doc.Delete(ctx); err != nil {
		return false, fmt.Errorf("delete %s/%s: %w", section, name, err)
	}
	return true, nil
}

func (s *firestoreStore) GetItem(ctx context.Context, section, name string) (json.RawMessage, bool, error) {
	snap, err := s.items().Doc(itemDocID(section, name)).Get(ctx)
	if err != nil {
		if status.Code(err) == codes.NotFound {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("get %s/%s: %w", section, name, err)
	}
	body, err := docBody(snap)
	if err != nil {
		return nil, false, fmt.Errorf("get %s/%s: %w", section, name, err)
	}
	return body, true, nil
}

func (s *firestoreStore) ListItems(ctx context.Context, section string) (map[string]json.RawMessage, error) {
	// An equality filter on a single field is served by the automatic
	// single-field index, so this needs no composite index to be created in the
	// project. Ordering is done in Go for the same reason: Where + OrderBy on
	// different fields would require one.
	iter := s.items().Where(fsFieldSection, "==", section).Documents(ctx)
	defer iter.Stop()

	out := make(map[string]json.RawMessage)
	for {
		snap, err := iter.Next()
		if errors.Is(err, iterator.Done) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("list %s: %w", section, err)
		}
		name, err := docString(snap, fsFieldName)
		if err != nil {
			return nil, fmt.Errorf("list %s: %w", section, err)
		}
		body, err := docBody(snap)
		if err != nil {
			return nil, fmt.Errorf("list %s/%s: %w", section, name, err)
		}
		out[name] = body
	}
	return out, nil
}

func (s *firestoreStore) PutSingleton(ctx context.Context, key string, body any) error {
	if !config.ValidSingleton(key) {
		return fmt.Errorf("unknown config singleton %q", key)
	}
	raw, err := marshalBody(body)
	if err != nil {
		return err
	}
	if _, err := s.singletons().Doc(key).Set(ctx, map[string]any{
		fsFieldKey:       key,
		fsFieldBody:      string(raw),
		fsFieldUpdatedAt: firestore.ServerTimestamp,
	}); err != nil {
		return fmt.Errorf("put singleton %s: %w", key, err)
	}
	return nil
}

func (s *firestoreStore) GetSingleton(ctx context.Context, key string) (json.RawMessage, bool, error) {
	snap, err := s.singletons().Doc(key).Get(ctx)
	if err != nil {
		if status.Code(err) == codes.NotFound {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("get singleton %s: %w", key, err)
	}
	body, err := docBody(snap)
	if err != nil {
		return nil, false, fmt.Errorf("get singleton %s: %w", key, err)
	}
	return body, true, nil
}

func (s *firestoreStore) Snapshot(ctx context.Context) (config.Snapshot, error) {
	var snap config.Snapshot
	for _, section := range config.AllSections {
		rows, err := s.ListItems(ctx, section)
		if err != nil {
			return config.Snapshot{}, err
		}
		names := make([]string, 0, len(rows))
		for name := range rows {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			snap.Items = append(snap.Items, config.SnapshotItem{Section: section, Name: name, Body: rows[name]})
		}
	}
	for _, key := range config.AllSingletons {
		body, ok, err := s.GetSingleton(ctx, key)
		if err != nil {
			return config.Snapshot{}, err
		}
		if ok {
			snap.Singletons = append(snap.Singletons, config.SnapshotSingleton{Key: key, Body: body})
		}
	}
	return snap, nil
}

// Restore writes every row from a snapshot, batching so a full config store
// crosses the network in a handful of round trips rather than one per entity.
func (s *firestoreStore) Restore(ctx context.Context, snap config.Snapshot) error {
	writes := make([]func(*firestore.WriteBatch), 0, len(snap.Items)+len(snap.Singletons))

	for _, item := range snap.Items {
		if !config.ValidSection(item.Section) {
			return fmt.Errorf("unknown config section %q", item.Section)
		}
		if strings.TrimSpace(item.Name) == "" {
			return errors.New("config item name must not be blank")
		}
		raw, err := marshalBody(item.Body)
		if err != nil {
			return err
		}
		doc := s.items().Doc(itemDocID(item.Section, item.Name))
		data := map[string]any{
			fsFieldSection:   item.Section,
			fsFieldName:      item.Name,
			fsFieldBody:      string(raw),
			fsFieldUpdatedAt: firestore.ServerTimestamp,
		}
		writes = append(writes, func(b *firestore.WriteBatch) { b.Set(doc, data) })
	}

	for _, single := range snap.Singletons {
		if !config.ValidSingleton(single.Key) {
			return fmt.Errorf("unknown config singleton %q", single.Key)
		}
		raw, err := marshalBody(single.Body)
		if err != nil {
			return err
		}
		doc := s.singletons().Doc(single.Key)
		data := map[string]any{
			fsFieldKey:       single.Key,
			fsFieldBody:      string(raw),
			fsFieldUpdatedAt: firestore.ServerTimestamp,
		}
		writes = append(writes, func(b *firestore.WriteBatch) { b.Set(doc, data) })
	}

	for start := 0; start < len(writes); start += firestoreBatchLimit {
		end := min(start+firestoreBatchLimit, len(writes))
		if err := s.commitBatch(ctx, writes[start:end]); err != nil {
			return err
		}
	}
	return nil
}

// commitBatch commits one chunk of at most firestoreBatchLimit writes. Chunks
// are atomic individually but not collectively: a Restore that spans several
// chunks and fails partway leaves the earlier ones applied, exactly as the SQL
// backends' per-row Restore does.
func (s *firestoreStore) commitBatch(ctx context.Context, writes []func(*firestore.WriteBatch)) error {
	batch := s.client.Batch()
	for _, apply := range writes {
		apply(batch)
	}
	if _, err := batch.Commit(ctx); err != nil {
		return fmt.Errorf("restore config store: %w", err)
	}
	return nil
}

// docBody extracts and validates the JSON body field of a config document.
func docBody(snap *firestore.DocumentSnapshot) (json.RawMessage, error) {
	body, err := docString(snap, fsFieldBody)
	if err != nil {
		return nil, err
	}
	return compactJSON([]byte(body))
}

// docString reads one string field, reporting a clear error for a document that
// is missing it or holds the wrong type — a hand-edited console entry, most
// likely, which is worth naming precisely rather than surfacing as a nil panic.
func docString(snap *firestore.DocumentSnapshot, field string) (string, error) {
	raw, err := snap.DataAt(field)
	if err != nil {
		return "", fmt.Errorf("document %s: missing field %q", snap.Ref.ID, field)
	}
	value, ok := raw.(string)
	if !ok {
		return "", fmt.Errorf("document %s: field %q is %T, want string", snap.Ref.ID, field, raw)
	}
	return value, nil
}
