# PebbleDB

PebbleDB is a simple, in-memory key-value database built in Go. It provides a basic command-line interface (CLI) for performing CRUD (Create, Read, Update, Delete) operations on key-value pairs. This toy database is designed to help users understand the core concepts of databases and key-value storage systems.

## Features

- **Set**: Store key-value pairs.
- **Get**: Retrieve values by keys.
- **Delete**: Remove key-value pairs.
- **Exit**: Exit the application.

## Getting Started

### Prerequisites

- Go 1.16+ installed on your machine.

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


