#!/bin/bash

# Configuration
CONFIG_FILE="viiwork.yaml"
BACKUP_FILE="${CONFIG_FILE}.bak"

# Determine sed command based on OS
if [[ "$OSTYPE" == "darwin"* ]]; then
    # macOS requires an empty string argument for -i to work
    SED_INPLACE="sed -i '' -E"
else
    # Linux/Unix
    SED_INPLACE="sed -i -E"
fi

# Function to escape characters for sed replacement string
escape_for_sed() {
    printf '%s' "$1" | sed 's/[\\&]/\\&/g'
}

# Check if model path is provided as argument
if [ $# -gt 0 ]; then
    NEW_PATH="$1"
else
    echo "Enter the full path to the model file:"
    read -r NEW_PATH
fi

# Validate input
if [ -z "$NEW_PATH" ]; then
    echo "Error: Model path cannot be empty."
    exit 1
fi

# Check if config file exists
if [ ! -f "$CONFIG_FILE" ]; then
    echo "Error: Configuration file '$CONFIG_FILE' not found."
    exit 1
fi

# Create a backup
cp "$CONFIG_FILE" "$BACKUP_FILE"

# Prepare the escaped path for safe sed insertion
SAFE_PATH=$(escape_for_sed "$NEW_PATH")

# Perform the replacement (note the -E flag for backreference support)
echo "Updating model path to: $NEW_PATH"
${SED_INPLACE} "s#^([[:space:]]*path:[[:space:]]*).*#\1${SAFE_PATH}#" "$CONFIG_FILE"

# Verify the change
if grep -q "^ *path:.*${NEW_PATH}" "$CONFIG_FILE"; then
    echo "SUCCESS: Path updated in '$CONFIG_FILE'"
    vim $CONFIG_FILE;
    echo "Restarting docker with 'docker compose down && docker compose up -d'"
    sudo docker compose down && sudo docker compose up -d
else
    echo "WARNING: Could not verify the path update. Check if 'path' exists in the file."
    cp "$BACKUP_FILE" "$CONFIG_FILE"
    exit 1
fi

echo "Done."
