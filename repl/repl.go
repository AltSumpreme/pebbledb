package repl

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"pebbledb"
	"pebbledb/parser"
	"strings"
)

func ReplInit(engine *pebbledb.Engine) {

	reader := bufio.NewReader(os.Stdin)
	fmt.Println("Welcome to PebbleDB ! Type 'Exit' to quit.")

	for {
		fmt.Print("> ")
		input, err := reader.ReadString('\n')
		if err != nil && !errorsIsEOF(err) {
			fmt.Printf("Error reading command: %s\n", err)
			continue
		}
		input = strings.TrimSpace(input)
		if input == "" {
			if errorsIsEOF(err) {
				fmt.Println("Exiting PebbleDB. Goodbye!")
				break
			}
			fmt.Println("Please enter a command.")
			continue
		}
		if strings.ToUpper(input) == "EXIT" {
			fmt.Println("Exiting PebbleDB. Goodbye!")
			break
		}

		cmd, err := parser.Parse(input)
		if err != nil {
			fmt.Printf("Error parsing command: %s\n", err)
			continue
		}
		if cmd == nil {
			fmt.Println("No command detected. Please try again.")
			continue
		}

		result := engine.Execute(cmd)
		if result.Error != nil {
			fmt.Printf("Error executing command: %s\n", result.Error)
			continue
		} else if len(result.Rows) > 0 {
			for _, row := range result.Rows {
				for colName, value := range row {
					fmt.Printf("%s: %v ", colName, value)
				}
				fmt.Println()
			}
		} else if result.Message != "" {
			fmt.Println(result.Message)
			continue
		}
		fmt.Println("Command executed successfully.")
		if errorsIsEOF(err) {
			break
		}
	}

}

func errorsIsEOF(err error) bool {
	return err == io.EOF
}
