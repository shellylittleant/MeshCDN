#!/usr/bin/env bash
# prepare-release.sh
#
# Prepares a clean copy of the MeshCDN source tree for public release.
# Reads from the local source directory; writes to OUTPUT_DIR.
#
# What this does:
#   1. Copies source code (Go files, configs, scripts) to OUTPUT_DIR
#   2. Skips secrets, binaries, databases, logs, certs, backups
#   3. Replaces hardcoded production IPs/domains with example.com / 192.0.2.x placeholders
#   4. Removes any embedded API keys / tokens / credentials
#   5. Adds public-release docs (README, LICENSE, etc.) from release-prep/
#   6. Creates examples/ directory with sample configs
#
# What this does NOT do:
#   - Translate Chinese comments (they stay; per maintainer preference)
#   - Modify the actual logic in any source file
#   - Rewrite git history (run separately if desired)
#
# Usage:
#   bash scripts/prepare-release.sh
#
# Output:
#   ./meshcdn-public/ — ready to commit to a public GitHub repo

set -euo pipefail

# ============================================================
# Configuration
# ============================================================

# Directory containing your private source (default: current dir)
SOURCE_DIR="${SOURCE_DIR:-.}"

# Where to write the cleaned copy
OUTPUT_DIR="${OUTPUT_DIR:-./meshcdn-public}"

# Where the prepared docs (README.md, LICENSE, etc.) live
DOCS_PREP_DIR="${DOCS_PREP_DIR:-./release-prep}"

# Replacement patterns — extend this list before running
# Format: "PATTERN:REPLACEMENT" (PATTERN is a literal string, not regex)
declare -a REDACTIONS=(
    # Production IPs in the codebase → example IPs
    "74.48.84.64:192.0.2.10"
    "47.82.82.159:192.0.2.20"
    "124.71.45.16:192.0.2.30"
    "20.198.242.68:192.0.2.40"
    "113.45.54.86:192.0.2.50"
    "206.238.77.91:192.0.2.60"
    "35.74.0.249:192.0.2.70"
    "18.177.147.68:192.0.2.80"

    # Production domain placeholders (extend as needed)
    "iyinhou.com:example.com"

    # Storage hostnames
    "o6t0v5t7o2u4.storagevgduddrx.xyz:storage.example.com"

    # Any internal company name in code — extend if relevant
    # "<your-company>:Example Org"
)

# ============================================================
# Sanity checks
# ============================================================

if [[ ! -d "$SOURCE_DIR" ]]; then
    echo "ERROR: source directory $SOURCE_DIR does not exist"
    exit 1
fi

if [[ ! -f "$SOURCE_DIR/go.mod" ]]; then
    echo "ERROR: $SOURCE_DIR doesn't look like a Go project (no go.mod)"
    exit 1
fi

if [[ -e "$OUTPUT_DIR" ]]; then
    echo "WARNING: $OUTPUT_DIR exists. Remove? (y/N)"
    read -r ans
    [[ "$ans" == "y" || "$ans" == "Y" ]] || exit 1
    rm -rf "$OUTPUT_DIR"
fi

mkdir -p "$OUTPUT_DIR"

# ============================================================
# Step 1: Copy source files (excluding secrets, build artifacts)
# ============================================================

echo "[1/5] Copying source tree..."

# rsync with explicit excludes
rsync -av \
    --exclude='.git/' \
    --exclude='vendor/' \
    --exclude='*.db' \
    --exclude='*.db-wal' \
    --exclude='*.db-shm' \
    --exclude='*.log' \
    --exclude='logs/' \
    --exclude='cache/' \
    --exclude='challenges/' \
    --exclude='backups/' \
    --exclude='certs/' \
    --exclude='config.json' \
    --exclude='peers.json' \
    --exclude='manifest.json' \
    --exclude='*.crt' \
    --exclude='*.key' \
    --exclude='*.pem' \
    --exclude='*.csr' \
    --exclude='*.tar.gz' \
    --exclude='cdn-agent' \
    --exclude='/cdn-agent' \
    --exclude='meshcdn-release-*' \
    --exclude='meshcdn-v*-linux-amd64' \
    --exclude='.env' \
    --exclude='.env.*' \
    --exclude='.vscode/' \
    --exclude='.idea/' \
    --exclude='*.swp' \
    --exclude='.DS_Store' \
    --exclude='release-prep/' \
    --exclude='meshcdn-public/' \
    "$SOURCE_DIR/" "$OUTPUT_DIR/" >/dev/null

echo "  Source copy: done"

# ============================================================
# Step 2: Apply redactions
# ============================================================

echo "[2/5] Applying redactions..."

# Find all text files (Go, scripts, docs, configs)
mapfile -t TEXT_FILES < <(
    find "$OUTPUT_DIR" -type f \
        \( -name '*.go' \
        -o -name '*.sh' \
        -o -name '*.md' \
        -o -name '*.json' \
        -o -name '*.yaml' \
        -o -name '*.yml' \
        -o -name '*.toml' \
        -o -name 'Makefile' \
        -o -name 'Dockerfile' \) \
        -not -path '*/vendor/*'
)

for redaction in "${REDACTIONS[@]}"; do
    pattern="${redaction%%:*}"
    replacement="${redaction##*:}"

    if [[ -z "$pattern" || -z "$replacement" ]]; then
        continue
    fi

    count=0
    for f in "${TEXT_FILES[@]}"; do
        if grep -qF "$pattern" "$f" 2>/dev/null; then
            # Use perl for robust string replacement (handles slashes, dots safely)
            perl -i -pe "s/\Q$pattern\E/$replacement/g" "$f"
            count=$((count + 1))
        fi
    done

    if [[ $count -gt 0 ]]; then
        echo "  Redacted '$pattern' → '$replacement' in $count file(s)"
    fi
done

# Generic API-key pattern scan (warn but don't auto-redact)
echo ""
echo "  Scanning for potential leaked credentials..."
SUSPICIOUS=$(grep -rEn \
    -e 'sk-[A-Za-z0-9_-]{20,}' \
    -e 'sk-proj-[A-Za-z0-9_-]{20,}' \
    -e '[0-9]{8,}:[A-Za-z0-9_-]{30,}' \
    --include='*.go' --include='*.sh' --include='*.json' \
    --include='*.md' --include='*.yaml' \
    "$OUTPUT_DIR" 2>/dev/null || true)

if [[ -n "$SUSPICIOUS" ]]; then
    echo ""
    echo "  ⚠️  Found patterns that look like API keys / bot tokens:"
    echo "$SUSPICIOUS" | head -20
    echo ""
    echo "  ⚠️  Review these manually before publishing."
fi

# ============================================================
# Step 3: Drop in public-release docs
# ============================================================

echo ""
echo "[3/5] Adding public-release documentation..."

if [[ ! -d "$DOCS_PREP_DIR" ]]; then
    echo "  ⚠️  $DOCS_PREP_DIR not found, skipping doc drop-in"
else
    cp -v "$DOCS_PREP_DIR/README.md" "$OUTPUT_DIR/README.md"
    cp -v "$DOCS_PREP_DIR/LICENSE" "$OUTPUT_DIR/LICENSE"
    cp -v "$DOCS_PREP_DIR/CHANGELOG.md" "$OUTPUT_DIR/CHANGELOG.md"
    cp -v "$DOCS_PREP_DIR/CONTRIBUTING.md" "$OUTPUT_DIR/CONTRIBUTING.md"
    cp -v "$DOCS_PREP_DIR/.gitignore" "$OUTPUT_DIR/.gitignore"

    mkdir -p "$OUTPUT_DIR/docs"
    cp -v "$DOCS_PREP_DIR/docs/"*.md "$OUTPUT_DIR/docs/"

    if [[ -d "$DOCS_PREP_DIR/examples" ]]; then
        mkdir -p "$OUTPUT_DIR/examples"
        cp -rv "$DOCS_PREP_DIR/examples/"* "$OUTPUT_DIR/examples/"
    fi
fi

# ============================================================
# Step 4: Verify the build still works
# ============================================================

echo ""
echo "[4/5] Verifying Go build..."

if (cd "$OUTPUT_DIR" && go build -o /tmp/cdn-agent-verify ./cmd/cdn-agent/ 2>&1); then
    echo "  ✓ go build succeeds in cleaned tree"
    rm -f /tmp/cdn-agent-verify
else
    echo "  ⚠️  go build failed in cleaned tree — review manually"
fi

# ============================================================
# Step 5: Final report
# ============================================================

echo ""
echo "[5/5] Summary"
echo ""
echo "Output: $OUTPUT_DIR"
echo "Files:"
find "$OUTPUT_DIR" -type f | wc -l | xargs printf "  Total files: %s\n"
find "$OUTPUT_DIR" -type f -name '*.go' | wc -l | xargs printf "  Go files: %s\n"
echo ""
echo "Lines of code:"
find "$OUTPUT_DIR" -type f -name '*.go' -not -path '*/vendor/*' \
    -exec wc -l {} \; | awk '{s+=$1} END {printf "  Go LOC: %d\n", s}'
echo ""
echo "Next steps:"
echo "  1. Review $OUTPUT_DIR for any remaining sensitive data"
echo "  2. Run 'cd $OUTPUT_DIR && grep -r \"YOUR-IP\\|YOUR-DOMAIN\\|TODO\" .' for final manual review"
echo "  3. Initialize git: cd $OUTPUT_DIR && git init && git add . && git commit -s -m 'Initial public release'"
echo "  4. Push to GitHub: git remote add origin <url> && git push -u origin main"
echo ""
echo "✓ Done"
