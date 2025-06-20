# PebbleDB

PebbleDB is a simple,lightweight modular database being built in Go. It provides a SQL-like interface,an in-memory engine,and disk persistence via JSON files.


> [!WARNING]
> 
> PebbleDB is a WIP
> 
> Most aspects of the project are under heavy development
> and no stable release is present as of yet.
> 

## Features

- **SQL-like REPL**: SQL-like command parsing and execution
- **Engine Abstraction** :  Engine handles orchestration between parser, executor, and storage.
- **JSON Persistence** : Data persists to disk in JSON format.
- **Page Management** : Introduced fixed-size 4KB page abstraction.
- **Test Coverage** : Initial unit tests for core DB operations.



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

- **CREATE**: 
```bash
CREATE TABLE table_name column_name:column type
```
- **INSERT**
```bash
INSERT TO TABLE table_name (col1,col2) FROM VALUES (val1,val2)
```
- **SELECT**


-To Select ALL Columns
```bash
SELECT * FROM table_name
```

- Select Specific Columns

```bash
SELECT col1,col2 FROM table_name
```

- **DROP**
```bash
DROP TABLE table_name
```
- **EXIT**: Exit the database.


## Future Plans

- [x] **Persistence Layer**: Add the ability to persist data
- [ ] **Adding a WAL**
- [ ] **Concurrency Support**: Add support for concurrent access with read-write locks.
- [ ] **Additional Commands**: Implement `UPDATE` and `LIST` commands.
- [ ] **Data Types**: Support storing complex data types like arrays and structs.
- [ ] **Performance Optimization**: Improve memory usage and introduce indexing.
- [ ] **CLI Enhancements**: Add interactive mode and command history.
- [ ] **Error Handling**: Enhance error handling for graceful recovery.
- [ ] **Testing and Documentation**: Add unit tests and comprehensive documentation.


#### Testing
```bash
cd testing
go test

```

**For more verbose output**
```bash
go test -v
```