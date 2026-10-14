package store

// KV is the narrow atomic storage boundary required from Mattermost plugin KV.
// Implementations must compare old values byte-for-byte.
type KV interface {
	Get(key string) ([]byte, error)
	Set(key string, value []byte) error
	SetWithExpiry(key string, value []byte, expireInSeconds int64) error
	CompareAndSet(key string, oldValue, newValue []byte) (bool, error)
	CompareAndSetWithExpiry(key string, oldValue, newValue []byte, expireInSeconds int64) (bool, error)
	CompareAndDelete(key string, oldValue []byte) (bool, error)
	Delete(key string) error
	List(prefix string, page, perPage int) ([]string, error)
}
