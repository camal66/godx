package storageclient

// PublicStorageClientAPI defines the object used to call eligible public APIs
// public APIs are used to acquire information
type PublicStorageClientAPI struct {
	sc *StorageClient
}

// NewPublicStorageClientAPI initialize PublicStorageClientAPI object
// which implemented a bunch of API methods
func NewPublicStorageClientAPI(sc *StorageClient) *PublicStorageClientAPI {
	return &PublicStorageClientAPI{sc}
}

// Payment returns the current payment settings
func (api *PublicStorageClientAPI) Payment() string {
	return "working in progress: getting payment information"
}

// MemoryAvailable returns current memory available
func (api *PublicStorageClientAPI) MemoryAvailable() uint64 {
	return api.sc.memoryManager.MemoryAvailable()
}

// MemoryLimit returns max memory allowed
func (api *PublicStorageClientAPI) MemoryLimit() uint64 {
	return api.sc.memoryManager.MemoryLimit()
}

// PrivateStorageClientAPI defines the object used to call eligible APIs
// private APIs are used to configure settings
type PrivateStorageClientAPI struct {
	sc *StorageClient
}

// NewPrivateStorageClientAPI initialize PrivateStorageClientAPI object
// which implemented a bunch of API methods
func NewPrivateStorageClientAPI(sc *StorageClient) *PrivateStorageClientAPI {
	return &PrivateStorageClientAPI{sc}
}

// SetPayment allows user to configure storage payment settings
// which will be used to select eligible the StorageHost
func (api *PrivateStorageClientAPI) SetPayment() string {
	return "working in progress: setting payment information"
}

// SetMemoryLimit allows user to expand or shrink the current memory limit
func (api *PrivateStorageClientAPI) SetMemoryLimit(amount uint64) string {
	return api.sc.memoryManager.SetMemoryLimit(amount)
}