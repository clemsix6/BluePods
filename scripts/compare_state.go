//go:build ignore

package main

import (
	"bytes"
	"fmt"
	"os"

	"BluePods/internal/storage"
)

func main() {
	if len(os.Args) != 3 {
		fmt.Fprintf(os.Stderr, "Usage: %s <db1_path> <db2_path>\n", os.Args[0])
		os.Exit(1)
	}

	db1Path := os.Args[1]
	db2Path := os.Args[2]

	db1, err := storage.New(db1Path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "open db1: %v\n", err)
		os.Exit(1)
	}
	defer db1.Close()

	db2, err := storage.New(db2Path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "open db2: %v\n", err)
		os.Exit(1)
	}
	defer db2.Close()

	// Collect objects from both DBs (only 32-byte keys = objects)
	objects1 := collectObjects(db1)
	objects2 := collectObjects(db2)

	fmt.Printf("DB1 (%s): %d objects\n", db1Path, len(objects1))
	fmt.Printf("DB2 (%s): %d objects\n", db2Path, len(objects2))

	// Compare
	missing1, missing2, different := compare(objects1, objects2)

	if len(missing1) == 0 && len(missing2) == 0 && len(different) == 0 {
		fmt.Println("\n✓ States are identical!")
		os.Exit(0)
	}

	fmt.Println("\n✗ States differ:")

	if len(missing1) > 0 {
		fmt.Printf("  - Objects in DB1 but not in DB2: %d\n", len(missing1))
		for _, id := range missing1 {
			fmt.Printf("      %x\n", id[:8])
		}
	}

	if len(missing2) > 0 {
		fmt.Printf("  - Objects in DB2 but not in DB1: %d\n", len(missing2))
		for _, id := range missing2 {
			fmt.Printf("      %x\n", id[:8])
		}
	}

	if len(different) > 0 {
		fmt.Printf("  - Objects with different content: %d\n", len(different))
		for _, id := range different {
			fmt.Printf("      %x\n", id[:8])
		}
	}

	os.Exit(1)
}

func collectObjects(db *storage.Storage) map[[32]byte][]byte {
	objects := make(map[[32]byte][]byte)

	db.Iterate(func(key, value []byte) error {
		// Skip non-object keys
		if len(key) != 32 {
			return nil
		}
		// Skip consensus prefixes
		if bytes.HasPrefix(key, []byte("v:")) || bytes.HasPrefix(key, []byte("r:")) || bytes.HasPrefix(key, []byte("m:")) {
			return nil
		}

		var id [32]byte
		copy(id[:], key)

		valueCopy := make([]byte, len(value))
		copy(valueCopy, value)
		objects[id] = valueCopy

		return nil
	})

	return objects
}

func compare(obj1, obj2 map[[32]byte][]byte) (missing1, missing2 [][32]byte, different [][32]byte) {
	// Find objects in obj1 but not in obj2
	for id := range obj1 {
		if _, ok := obj2[id]; !ok {
			missing1 = append(missing1, id)
		}
	}

	// Find objects in obj2 but not in obj1
	for id := range obj2 {
		if _, ok := obj1[id]; !ok {
			missing2 = append(missing2, id)
		}
	}

	// Find objects with different content
	for id, data1 := range obj1 {
		if data2, ok := obj2[id]; ok {
			if !bytes.Equal(data1, data2) {
				different = append(different, id)
			}
		}
	}

	return
}
