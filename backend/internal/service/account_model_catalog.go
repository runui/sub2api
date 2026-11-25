package service

import "context"

const falCatalogSource = "fal_platform_api"

// GetAccountModelCatalog reads discovery only. Selecting a model, routing it,
// and pricing it are separate decisions.
func (s *AccountTestService) GetAccountModelCatalog(ctx context.Context, account *Account) (*UpstreamModelCatalog, error) {
	empty := &UpstreamModelCatalog{Models: []string{}}
	if account.Platform != PlatformFal {
		return empty, nil
	}
	repo, ok := s.accountRepo.(accountModelCatalogSnapshotReader)
	if !ok {
		return empty, nil
	}
	catalog, err := repo.GetAccountModelCatalogSnapshot(ctx, account.ID, falCatalogSource)
	if err != nil {
		return nil, newUpstreamModelSyncInternalError("Failed to read upstream model catalog", err)
	}
	if catalog == nil {
		return empty, nil
	}
	return catalog, nil
}
