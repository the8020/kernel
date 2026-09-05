package packages

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Catalog is the read-only view of activated package source. It is available
// before shared database state so fresh-database table evaluation has no
// circular dependency on the package index.
type Catalog struct {
	packagesRoot string
	logger       *slog.Logger
}

func NewCatalog(packagesRoot string, logger *slog.Logger) (*Catalog, error) {
	root, err := canonicalDirectory(packagesRoot)
	if err != nil {
		return nil, fmt.Errorf("packages root: %w", err)
	}
	return &Catalog{packagesRoot: root, logger: logger}, nil
}

func (c *Catalog) PackagesRoot() string { return c.packagesRoot }

func (c *Catalog) ListPackages() ([]Package, error) {
	namespaces, err := os.ReadDir(c.packagesRoot)
	if err != nil {
		return nil, fmt.Errorf("read packages root: %w", err)
	}
	var result []Package
	for _, namespace := range namespaces {
		if strings.HasPrefix(namespace.Name(), ".") || (!namespace.IsDir() && namespace.Type()&os.ModeSymlink == 0) {
			continue
		}
		repositories, readErr := os.ReadDir(filepath.Join(c.packagesRoot, namespace.Name()))
		if readErr != nil {
			result = append(result, Package{ID: namespace.Name() + "/?", Path: filepath.Join(c.packagesRoot, namespace.Name()), ValidationErrors: []string{readErr.Error()}})
			continue
		}
		for _, repository := range repositories {
			if strings.HasPrefix(repository.Name(), ".") || (!repository.IsDir() && repository.Type()&os.ModeSymlink == 0) {
				continue
			}
			if _, statErr := os.Lstat(filepath.Join(c.packagesRoot, namespace.Name(), repository.Name(), "package.toml")); errors.Is(statErr, os.ErrNotExist) {
				continue
			}
			item := c.inspectPackage(Identity{Namespace: namespace.Name(), Repository: repository.Name()})
			result = append(result, item)
			if c.logger != nil {
				level := slog.LevelInfo
				if !item.Valid {
					level = slog.LevelWarn
				}
				c.logger.Log(context.Background(), level, "package discovered", "package_id", item.ID, "path", item.Path, "valid", item.Valid, "validation_errors", item.ValidationErrors)
			}
		}
	}
	sort.Slice(result, func(i, j int) bool { return result[i].ID < result[j].ID })
	return result, nil
}

func (c *Catalog) ResolvePackage(packageID string) (Package, error) {
	identity, err := ParsePackageID(packageID)
	if err != nil {
		return Package{}, err
	}
	result := c.inspectPackage(identity)
	if !result.Valid {
		return result, fmt.Errorf("package %s is invalid: %s", packageID, strings.Join(result.ValidationErrors, "; "))
	}
	return result, nil
}

func (c *Catalog) inspectPackage(identity Identity) Package {
	root := filepath.Join(c.packagesRoot, identity.Namespace, identity.Repository)
	result := Package{ID: identity.PackageID(), Path: root}
	if err := ValidateName(identity.Namespace); err != nil {
		result.ValidationErrors = append(result.ValidationErrors, "namespace: "+err.Error())
	}
	if err := ValidateName(identity.Repository); err != nil {
		result.ValidationErrors = append(result.ValidationErrors, "repository: "+err.Error())
	}
	canonical, err := canonicalWithin(root, c.packagesRoot)
	rootValid := err == nil && canonical == root
	if err != nil {
		result.ValidationErrors = append(result.ValidationErrors, fmt.Sprintf("%s: %v", root, err))
	} else if canonical != root {
		result.ValidationErrors = append(result.ValidationErrors, fmt.Sprintf("package root %s resolves through a symlink", root))
	}
	if rootValid {
		var manifest PackageManifest
		manifestPath := filepath.Join(root, "package.toml")
		if err := decodeTOMLWithin(manifestPath, root, &manifest); err != nil {
			result.ValidationErrors = append(result.ValidationErrors, fmt.Sprintf("%s: %v", manifestPath, err))
		} else {
			if manifest.Schema != packageManifestSchema {
				result.ValidationErrors = append(result.ValidationErrors, fmt.Sprintf("%s: schema must equal %d", manifestPath, packageManifestSchema))
			}
			result.Description, result.DocumentationURL, result.License = manifest.Description, manifest.DocumentationURL, manifest.License
		}

	}
	result.Valid = len(result.ValidationErrors) == 0
	return result
}
