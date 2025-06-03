# PebbleDB

PebbleDB is a simple, in-memory key-value database built in Go. It provides a basic command-line interface (CLI) for performing CRUD (Create, Read, Update, Delete) operations on key-value pairs. This toy database is designed to help users understand the core concepts of databases and key-value storage systems.

## Features

- **Set**: Store key-value pairs.
- **Get**: Retrieve values by keys.
- **Delete**: Remove key-value pairs.
- **Exit**: Exit the application.

## Getting Started

### Prerequisites

- Go 1.20+ installed on your machine.

### Installation

1. Clone the repository:

    ```bash
    git clone https://github.com/AltSumpreme/pebbledb.git
    cd pebbledb
    ```

2. Build and run the project:

    ```bash
    go run cmd/main.go
    ```

### Usage

Once the application is running, you can interact with it via the command line.

#### Commands

- **SET `<key>` `<value>`**: Store a key-value pair.
- **GET `<key>`**: Retrieve the value for the given key.
- **DELETE `<key>`**: Delete a key-value pair.
- **EXIT**: Exit the database.


## Future Plans

- [x] **Persistence Layer**: Add the ability to persist data
- [ ] **Concurrency Support**: Add support for concurrent access with read-write locks.
- [ ] **Additional Commands**: Implement `UPDATE` and `LIST` commands.
- [ ] **Data Types**: Support storing complex data types like arrays and structs.
- [ ] **Performance Optimization**: Improve memory usage and introduce indexing.
- [ ] **CLI Enhancements**: Add interactive mode and command history.
- [ ] **Error Handling**: Enhance error handling for graceful recovery.
- [ ] **Testing and Documentation**: Add unit tests and comprehensive documentation.
