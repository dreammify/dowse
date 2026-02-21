# Quickstart

## Install Dowse

```bash
curl -fsSL https://raw.githubusercontent.com/dreammify/dowse/main/scripts/install.sh | bash
```

## Install Language Servers

Install the language servers for the languages you work with:

**Go:**
```bash
go install golang.org/x/tools/gopls@latest
```

**Kotlin:**
```bash
brew install kotlin-lsp
```

**Java:**
```bash
brew install jdtls
```

**TypeScript:**
```bash
npm install -g typescript-language-server typescript
```

## Configure

Create `~/.dowse/config.toml` with the LSPs you installed:

```bash
mkdir -p ~/.dowse
cat > ~/.dowse/config.toml << 'EOF'
[[lsp]]
extensions = [".go"]
command = ["gopls", "serve"]

[[lsp]]
extensions = [".kt", ".kts"]
command = ["kotlin-lsp", "--stdio"]

[[lsp]]
extensions = [".java"]
command = ["jdtls"]

[[lsp]]
extensions = [".ts", ".tsx"]
command = ["typescript-language-server", "--stdio"]
EOF
```

## Run

```bash
# Start the daemon
dowse start

# Get diagnostics for a file
dowse diagnostics /path/to/project/src/Main.kt

# Stop the daemon
dowse stop
```
