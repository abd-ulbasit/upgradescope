# typed: false
# frozen_string_literal: true

# Rendered by script/render-formula.sh from the checksums.txt of upgradescope
# release v0.1.1, after its keyless cosign signature was verified
# (.github/workflows/update.yml). Do not edit by hand: edit formula.rb.tmpl.
class Upgradescope < Formula
  desc "Kubernetes upgrade-readiness scanner: removed APIs, EOL add-ons, skew"
  homepage "https://github.com/abd-ulbasit/upgradescope"
  license "Apache-2.0"

  on_macos do
    on_arm do
      url "https://github.com/abd-ulbasit/upgradescope/releases/download/v0.1.1/upgradescope_darwin_arm64.tar.gz"
      sha256 "654e26d44d71cb022dfde0677622b68f6abd990299cc8ac0cf9229e718c12974"
    end
    on_intel do
      url "https://github.com/abd-ulbasit/upgradescope/releases/download/v0.1.1/upgradescope_darwin_amd64.tar.gz"
      sha256 "0a9d847d26c2106795db456d3440c2ab9fe8b79eafef8819c6d0bd100acac07b"
    end
  end

  on_linux do
    on_arm do
      url "https://github.com/abd-ulbasit/upgradescope/releases/download/v0.1.1/upgradescope_linux_arm64.tar.gz"
      sha256 "4993ff904c4b91a1249f88580d8121a608b86f6be5006d814428a97eb259d782"
    end
    on_intel do
      url "https://github.com/abd-ulbasit/upgradescope/releases/download/v0.1.1/upgradescope_linux_amd64.tar.gz"
      sha256 "96150a9ade386a189e395e45992f4dc56b5c9cdf5fe8efbc821baa77aa8518a6"
    end
  end

  def install
    bin.install "upgradescope"
    # The archive's completions/ and manpages/ are rendered from the same
    # command tree; generating completions from the binary also covers
    # releases that predate them.
    generate_completions_from_executable(bin/"upgradescope", "completion")
    man1.install Dir["manpages/*.1.gz"]
  end

  test do
    assert_match "upgradescope #{version}", shell_output("#{bin}/upgradescope version")
  end
end
