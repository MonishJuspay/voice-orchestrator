#!/bin/bash
set -e

GO_VERSION="1.25.7"
OS=$(uname -s | tr '[:upper:]' '[:lower:]')
ARCH=$(uname -m)

# Convert architecture naming
case $ARCH in
    x86_64)
        ARCH="amd64"
        ;;
    aarch64|arm64)
        ARCH="arm64"
        ;;
    *)
        echo "Unsupported architecture: $ARCH"
        exit 1
        ;;
esac

echo "==> Installing Go ${GO_VERSION} for ${OS}-${ARCH}"

# Download Go
DOWNLOAD_URL="https://go.dev/dl/go${GO_VERSION}.${OS}-${ARCH}.tar.gz"
TEMP_FILE="/tmp/go${GO_VERSION}.tar.gz"

echo "==> Downloading from ${DOWNLOAD_URL}..."
curl -L -o "$TEMP_FILE" "$DOWNLOAD_URL"

# Remove existing Go installation
if [ -d "/usr/local/go" ]; then
    echo "==> Removing existing Go installation..."
    sudo rm -rf /usr/local/go
fi

# Extract new Go installation
echo "==> Extracting Go..."
sudo tar -C /usr/local -xzf "$TEMP_FILE"

# Clean up
rm "$TEMP_FILE"

# Add to PATH if not already present
if ! grep -q "/usr/local/go/bin" ~/.bashrc 2>/dev/null; then
    echo "==> Adding Go to PATH in ~/.bashrc..."
    echo 'export PATH=$PATH:/usr/local/go/bin' >> ~/.bashrc
fi

if ! grep -q "/usr/local/go/bin" ~/.zshrc 2>/dev/null; then
    echo "==> Adding Go to PATH in ~/.zshrc..."
    echo 'export PATH=$PATH:/usr/local/go/bin' >> ~/.zshrc
fi

echo "==> Go ${GO_VERSION} installed successfully!"
echo ""
echo "Please restart your shell or run:"
echo "  export PATH=\$PATH:/usr/local/go/bin"
echo ""
echo "Verify installation with:"
echo "  go version"
