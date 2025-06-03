package main

import (
	"bufio"
	"fmt"
	"os"
	"pebbledb"
	"strings"
)

func main() {
	store := pebbledb.NewStore()

	go pebbledb.WaitForShutdown(pebbledb.Cleanup(store))

	fmt.Println("Welcome to PebbleDB! Type Exit to quit.")
	reader := bufio.NewReader(os.Stdin)
	for {

		fmt.Print("> ")
		line, _ := reader.ReadString('\n')
		line = line[:len(line)-1]
		if strings.ToUpper(line) == "EXIT" {
			fmt.Println("Exiting PebbleDB. Goodbye!")
			break
		}
		if len(line) == 0 {
			fmt.Println("Please enter a command.")
			continue
		}
		parts := strings.Split(line, " ")
		if len(parts) < 1 {
			fmt.Println("Invalid command. Please use SET, GET, DELETE, or EXIT.")
			continue
		}
		command := strings.ToUpper(parts[0])

		switch command {
		case "SET":
			if len(parts) != 3 {
				fmt.Println("Usage: SET <key> <value>")
				continue
			}
			key := parts[1]
			value := parts[2]
			store.Set(key, value)
			fmt.Printf("Set %s to %s\n", key, value)

		case "GET":
			if len(parts) != 2 {
				fmt.Println("Usage: GET <key>")
				continue
			}
			key := parts[1]
			value, exists := store.Get(key)
			if !exists {
				fmt.Printf("Key %s not found\n", key)
			} else {
				fmt.Printf("Value for %s is %s\n", key, value)
			}
		case "DELETE":
			if len(parts) != 2 {
				fmt.Print("Usage: DELETE <key>")
				continue
			}
			key := parts[1]
			if store.Delete(key) {
				fmt.Printf("Deleted key %s\n", key)
			} else {
				fmt.Printf("Key %s not found\n", key)
			}
		default:
			fmt.Println("Invalid command. Please use SET, GET, DELETE, or EXIT.")
		}

	}
}
