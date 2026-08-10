// Package lexer tokenizes SQL while preserving source positions.
package lexer

import (
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"
)

type Kind uint8

const (
	EOF Kind = iota
	Identifier
	Number
	String
	Comma
	Dot
	LeftParen
	RightParen
	Semicolon
	Star
	Plus
	Minus
	Slash
	Percent
	Equal
	NotEqual
	Less
	LessEqual
	Greater
	GreaterEqual
)

type Position struct {
	Offset int
	Line   int
	Column int
}

type Token struct {
	Kind     Kind
	Literal  string
	Quoted   bool
	Position Position
}

func Tokenize(input string) ([]Token, error) {
	if !utf8.ValidString(input) {
		return nil, fmt.Errorf("sql lexer: input is not valid UTF-8")
	}
	scanner := scanner{input: input, line: 1, column: 1}
	var tokens []Token
	for {
		token, err := scanner.next()
		if err != nil {
			return nil, err
		}
		tokens = append(tokens, token)
		if token.Kind == EOF {
			return tokens, nil
		}
	}
}

type scanner struct {
	input  string
	offset int
	line   int
	column int
}

func (scanner *scanner) next() (Token, error) {
	if err := scanner.skipTrivia(); err != nil {
		return Token{}, err
	}
	position := scanner.position()
	if scanner.offset >= len(scanner.input) {
		return Token{Kind: EOF, Position: position}, nil
	}
	current, _ := utf8.DecodeRuneInString(scanner.input[scanner.offset:])
	if isIdentifierStart(current) {
		start := scanner.offset
		for scanner.offset < len(scanner.input) {
			r, _ := utf8.DecodeRuneInString(scanner.input[scanner.offset:])
			if !isIdentifierContinue(r) {
				break
			}
			scanner.advanceRune(r)
		}
		return Token{Kind: Identifier, Literal: strings.ToLower(scanner.input[start:scanner.offset]), Position: position}, nil
	}
	if unicode.IsDigit(current) {
		return scanner.scanNumber(position)
	}
	switch current {
	case '"':
		return scanner.scanQuotedIdentifier(position)
	case '\'':
		return scanner.scanString(position)
	case ',':
		scanner.advanceRune(current)
		return Token{Kind: Comma, Literal: ",", Position: position}, nil
	case '.':
		scanner.advanceRune(current)
		return Token{Kind: Dot, Literal: ".", Position: position}, nil
	case '(':
		scanner.advanceRune(current)
		return Token{Kind: LeftParen, Literal: "(", Position: position}, nil
	case ')':
		scanner.advanceRune(current)
		return Token{Kind: RightParen, Literal: ")", Position: position}, nil
	case ';':
		scanner.advanceRune(current)
		return Token{Kind: Semicolon, Literal: ";", Position: position}, nil
	case '*':
		scanner.advanceRune(current)
		return Token{Kind: Star, Literal: "*", Position: position}, nil
	case '+':
		scanner.advanceRune(current)
		return Token{Kind: Plus, Literal: "+", Position: position}, nil
	case '-':
		scanner.advanceRune(current)
		return Token{Kind: Minus, Literal: "-", Position: position}, nil
	case '/':
		scanner.advanceRune(current)
		return Token{Kind: Slash, Literal: "/", Position: position}, nil
	case '%':
		scanner.advanceRune(current)
		return Token{Kind: Percent, Literal: "%", Position: position}, nil
	case '=':
		scanner.advanceRune(current)
		return Token{Kind: Equal, Literal: "=", Position: position}, nil
	case '!':
		scanner.advanceRune(current)
		if scanner.consume('=') {
			return Token{Kind: NotEqual, Literal: "!=", Position: position}, nil
		}
	case '<':
		scanner.advanceRune(current)
		if scanner.consume('=') {
			return Token{Kind: LessEqual, Literal: "<=", Position: position}, nil
		}
		if scanner.consume('>') {
			return Token{Kind: NotEqual, Literal: "<>", Position: position}, nil
		}
		return Token{Kind: Less, Literal: "<", Position: position}, nil
	case '>':
		scanner.advanceRune(current)
		if scanner.consume('=') {
			return Token{Kind: GreaterEqual, Literal: ">=", Position: position}, nil
		}
		return Token{Kind: Greater, Literal: ">", Position: position}, nil
	}
	return Token{}, scanner.errorAt(position, "unexpected character %q", current)
}

func (scanner *scanner) scanNumber(position Position) (Token, error) {
	start := scanner.offset
	for scanner.offset < len(scanner.input) {
		r, _ := utf8.DecodeRuneInString(scanner.input[scanner.offset:])
		if !unicode.IsDigit(r) {
			break
		}
		scanner.advanceRune(r)
	}
	if scanner.offset < len(scanner.input) && scanner.input[scanner.offset] == '.' {
		scanner.advanceRune('.')
		if scanner.offset >= len(scanner.input) || !unicode.IsDigit(rune(scanner.input[scanner.offset])) {
			return Token{}, scanner.errorAt(position, "decimal point must be followed by digits")
		}
		for scanner.offset < len(scanner.input) {
			r, _ := utf8.DecodeRuneInString(scanner.input[scanner.offset:])
			if !unicode.IsDigit(r) {
				break
			}
			scanner.advanceRune(r)
		}
	}
	return Token{Kind: Number, Literal: scanner.input[start:scanner.offset], Position: position}, nil
}

func (scanner *scanner) scanQuotedIdentifier(position Position) (Token, error) {
	scanner.advanceRune('"')
	var result strings.Builder
	for scanner.offset < len(scanner.input) {
		r, _ := utf8.DecodeRuneInString(scanner.input[scanner.offset:])
		scanner.advanceRune(r)
		if r != '"' {
			result.WriteRune(r)
			continue
		}
		if scanner.consume('"') {
			result.WriteRune('"')
			continue
		}
		if result.Len() == 0 {
			return Token{}, scanner.errorAt(position, "quoted identifier cannot be empty")
		}
		return Token{Kind: Identifier, Literal: result.String(), Quoted: true, Position: position}, nil
	}
	return Token{}, scanner.errorAt(position, "unterminated quoted identifier")
}

func (scanner *scanner) scanString(position Position) (Token, error) {
	scanner.advanceRune('\'')
	var result strings.Builder
	for scanner.offset < len(scanner.input) {
		r, _ := utf8.DecodeRuneInString(scanner.input[scanner.offset:])
		scanner.advanceRune(r)
		if r != '\'' {
			result.WriteRune(r)
			continue
		}
		if scanner.consume('\'') {
			result.WriteRune('\'')
			continue
		}
		return Token{Kind: String, Literal: result.String(), Position: position}, nil
	}
	return Token{}, scanner.errorAt(position, "unterminated string literal")
}

func (scanner *scanner) skipTrivia() error {
	for scanner.offset < len(scanner.input) {
		r, _ := utf8.DecodeRuneInString(scanner.input[scanner.offset:])
		if unicode.IsSpace(r) {
			scanner.advanceRune(r)
			continue
		}
		if strings.HasPrefix(scanner.input[scanner.offset:], "--") {
			for scanner.offset < len(scanner.input) {
				r, _ = utf8.DecodeRuneInString(scanner.input[scanner.offset:])
				scanner.advanceRune(r)
				if r == '\n' {
					break
				}
			}
			continue
		}
		if strings.HasPrefix(scanner.input[scanner.offset:], "/*") {
			start := scanner.position()
			scanner.advanceRune('/')
			scanner.advanceRune('*')
			for scanner.offset < len(scanner.input) && !strings.HasPrefix(scanner.input[scanner.offset:], "*/") {
				r, _ = utf8.DecodeRuneInString(scanner.input[scanner.offset:])
				scanner.advanceRune(r)
			}
			if scanner.offset >= len(scanner.input) {
				return scanner.errorAt(start, "unterminated block comment")
			}
			scanner.advanceRune('*')
			scanner.advanceRune('/')
			continue
		}
		break
	}
	return nil
}

func (scanner *scanner) consume(expected byte) bool {
	if scanner.offset < len(scanner.input) && scanner.input[scanner.offset] == expected {
		scanner.advanceRune(rune(expected))
		return true
	}
	return false
}

func (scanner *scanner) advanceRune(r rune) {
	scanner.offset += utf8.RuneLen(r)
	if r == '\n' {
		scanner.line++
		scanner.column = 1
	} else {
		scanner.column++
	}
}

func (scanner *scanner) position() Position {
	return Position{Offset: scanner.offset, Line: scanner.line, Column: scanner.column}
}

func (scanner *scanner) errorAt(position Position, format string, arguments ...any) error {
	return fmt.Errorf("sql lexer at %d:%d: %s", position.Line, position.Column, fmt.Sprintf(format, arguments...))
}

func isIdentifierStart(r rune) bool {
	return r == '_' || unicode.IsLetter(r)
}

func isIdentifierContinue(r rune) bool {
	return isIdentifierStart(r) || unicode.IsDigit(r) || r == '$'
}
