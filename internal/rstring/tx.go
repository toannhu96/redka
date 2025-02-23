package rstring

import (
	"database/sql"
	"slices"
	"time"

	"github.com/nalgeon/redka/internal/core"
	"github.com/nalgeon/redka/internal/rkey"
	"github.com/nalgeon/redka/internal/sqlx"
)

const (
	sqlGet = `
	select value
	from rstring
	  join rkey on key_id = rkey.id and (etime is null or etime > :now)
	where key = :key`

	sqlGetMany = `
	select key, value
	from rstring
	  join rkey on key_id = rkey.id and (etime is null or etime > :now)
	where key in (:keys)`

	sqlSet1 = `
	insert into rkey (key, type, version, etime, mtime)
	values (:key, :type, :version, :etime, :mtime)
	on conflict (key) do update set
	  version = version+1,
	  type = excluded.type,
	  etime = excluded.etime,
	  mtime = excluded.mtime`

	sqlSet2 = `
	insert into rstring (key_id, value)
	values ((select id from rkey where key = :key), :value)
	on conflict (key_id) do update
	set value = excluded.value`

	sqlUpdate1 = `
	insert into rkey (key, type, version, etime, mtime)
	values (:key, :type, :version, null, :mtime)
	on conflict (key) do update set
	  version = version+1,
	  type = excluded.type,
	  -- not changing etime
	  mtime = excluded.mtime`

	sqlUpdate2 = `
	insert into rstring (key_id, value)
	values ((select id from rkey where key = :key), :value)
	on conflict (key_id) do update
	set value = excluded.value`
)

// Tx is a string repository transaction.
type Tx struct {
	tx sqlx.Tx
}

// NewTx creates a string repository transaction
// from a generic database transaction.
func NewTx(tx sqlx.Tx) *Tx {
	return &Tx{tx}
}

// Get returns the value of the key.
// If the key does not exist or is not a string, returns ErrNotFound.
func (tx *Tx) Get(key string) (core.Value, error) {
	return get(tx.tx, key)
}

// GetMany returns a map of values for given keys.
// Ignores keys that do not exist or not strings,
// and does not return them in the map.
func (tx *Tx) GetMany(keys ...string) (map[string]core.Value, error) {
	// Get the values of the requested keys.
	now := time.Now().UnixMilli()
	query, keyArgs := sqlx.ExpandIn(sqlGetMany, ":keys", keys)
	args := slices.Concat([]any{sql.Named("now", now)}, keyArgs)

	var rows *sql.Rows
	rows, err := tx.tx.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	// Fill the map with the values for existing keys.
	items := map[string]core.Value{}
	for rows.Next() {
		var key string
		var val []byte
		err = rows.Scan(&key, &val)
		if err != nil {
			return nil, err
		}
		items[key] = core.Value(val)
	}
	if rows.Err() != nil {
		return nil, rows.Err()
	}

	return items, nil
}

// Incr increments the integer key value by the specified amount.
// Returns the value after the increment.
// If the key does not exist, sets it to 0 before the increment.
// If the key value is not an integer, returns ErrValueType.
// If the key exists but is not a string, returns ErrKeyType.
func (tx *Tx) Incr(key string, delta int) (int, error) {
	// get the current value
	val, err := tx.Get(key)
	if err != nil && err != core.ErrNotFound {
		return 0, err
	}

	// check if the value is a valid integer
	valInt, err := val.Int()
	if err != nil {
		return 0, core.ErrValueType
	}

	// increment the value
	newVal := valInt + delta
	err = update(tx.tx, key, newVal)
	if err != nil {
		return 0, err
	}

	return newVal, nil
}

// IncrFloat increments the float key value by the specified amount.
// Returns the value after the increment.
// If the key does not exist, sets it to 0 before the increment.
// If the key value is not an float, returns ErrValueType.
// If the key exists but is not a string, returns ErrKeyType.
func (tx *Tx) IncrFloat(key string, delta float64) (float64, error) {
	// get the current value
	val, err := tx.Get(key)
	if err != nil && err != core.ErrNotFound {
		return 0, err
	}

	// check if the value is a valid float
	valFloat, err := val.Float()
	if err != nil {
		return 0, core.ErrValueType
	}

	// increment the value
	newVal := valFloat + delta
	err = update(tx.tx, key, newVal)
	if err != nil {
		return 0, err
	}

	return newVal, nil
}

// Set sets the key value that will not expire.
// Overwrites the value if the key already exists.
// If the key exists but is not a string, returns ErrKeyType.
func (tx *Tx) Set(key string, value any) error {
	return tx.SetExpires(key, value, 0)
}

// SetExpires sets the key value with an optional expiration time (if ttl > 0).
// Overwrites the value and ttl if the key already exists.
// If the key exists but is not a string, returns ErrKeyType.
func (tx *Tx) SetExpires(key string, value any, ttl time.Duration) error {
	if !core.IsValueType(value) {
		return core.ErrValueType
	}
	err := set(tx.tx, key, value, ttl)
	return err
}

// SetMany sets the values of multiple keys.
// Overwrites values for keys that already exist and
// creates new keys/values for keys that do not exist.
// Removes the TTL for existing keys.
// If any of the keys exists but is not a string, returns ErrKeyType.
func (tx *Tx) SetMany(items map[string]any) error {
	for _, val := range items {
		if !core.IsValueType(val) {
			return core.ErrValueType
		}
	}

	for key, val := range items {
		err := set(tx.tx, key, val, 0)
		if err != nil {
			return err
		}
	}

	return nil
}

// SetManyNX sets the values of multiple keys, but only if none
// of them yet exist. Returns true if the keys were set,
// false if any of them already exist.
// If any of the keys exists but is not a string, returns ErrKeyType.
func (tx *Tx) SetManyNX(items map[string]any) (bool, error) {
	for _, val := range items {
		if !core.IsValueType(val) {
			return false, core.ErrValueType
		}
	}

	// extract keys
	keys := make([]string, 0, len(items))
	for key := range items {
		keys = append(keys, key)
	}

	// check if any of the keys exist
	count, err := rkey.CountType(tx.tx, core.TypeString, keys...)
	if err != nil {
		return false, err
	}

	// do not proceed if any of the keys exist
	if count != 0 {
		return false, nil
	}

	// set the keys
	for key, val := range items {
		err = set(tx.tx, key, val, 0)
		if err != nil {
			return false, err
		}
	}

	return true, nil
}

// SetWith sets the key value with additional options.
func (tx *Tx) SetWith(key string, value any) SetCmd {
	return SetCmd{tx: tx, key: key, val: value}
}

func get(tx sqlx.Tx, key string) (core.Value, error) {
	args := []any{
		sql.Named("key", key),
		sql.Named("now", time.Now().UnixMilli()),
	}
	var val []byte
	err := tx.QueryRow(sqlGet, args...).Scan(&val)
	if err == sql.ErrNoRows {
		return core.Value(nil), core.ErrNotFound
	}
	if err != nil {
		return core.Value(nil), err
	}
	return core.Value(val), nil
}

// set sets the key value and (optionally) its expiration time.
func set(tx sqlx.Tx, key string, value any, ttl time.Duration) error {
	now := time.Now()
	var etime *int64
	if ttl > 0 {
		etime = new(int64)
		*etime = now.Add(ttl).UnixMilli()
	}

	args := []any{
		sql.Named("key", key),
		sql.Named("type", core.TypeString),
		sql.Named("version", core.InitialVersion),
		sql.Named("etime", etime),
		sql.Named("mtime", now.UnixMilli()),
		sql.Named("value", value),
	}

	_, err := tx.Exec(sqlSet1, args...)
	if err != nil {
		return sqlx.TypedError(err)
	}

	_, err = tx.Exec(sqlSet2, args...)
	return err
}

// update updates the value of the existing key without changing its
// expiration time. If the key does not exist, creates a new key with
// the specified value and no expiration time.
func update(tx sqlx.Tx, key string, value any) error {
	now := time.Now().UnixMilli()
	args := []any{
		sql.Named("key", key),
		sql.Named("type", core.TypeString),
		sql.Named("version", core.InitialVersion),
		sql.Named("mtime", now),
		sql.Named("value", value),
	}
	_, err := tx.Exec(sqlUpdate1, args...)
	if err != nil {
		return sqlx.TypedError(err)
	}
	_, err = tx.Exec(sqlUpdate2, args...)
	return err
}
