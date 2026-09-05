package packages

import "path/filepath"

// EventListener is a flat TOML declaration referencing an ordinary program.
type EventListener struct {
	HandlerDefinition
	ID            string
	PackageID     string
	Event         string
	ProgramCommit string
}

func ValidateEventListeners(root, packageID string) ([]EventListener, error) {
	if _, err := ParsePackageID(packageID); err != nil {
		return nil, err
	}
	files, err := DeclarationFiles(root, "events")
	if err != nil {
		return nil, err
	}
	result := make([]EventListener, 0, len(files))
	for _, path := range files {
		handler, event, _, err := readHandler(root, path, "events")
		if err != nil {
			return nil, err
		}
		result = append(result, EventListener{ID: packageID + "/events/" + filepath.Base(path), PackageID: packageID, Event: event, HandlerDefinition: handler})
	}
	return result, nil
}
