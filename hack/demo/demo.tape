# upgradescope README demo — render from the repo root:
#   vhs hack/demo/demo.tape
# Requires: bin/upgradescope built (make build), kind-upgradescope-demo
# cluster running (hack/demo/kind-setup.sh) and selected as current context.

Output docs/img/demo.gif

Set Shell bash
Set FontSize 13
Set Width 800
Set Height 600
Set Padding 16
Set Theme "Dracula"
Set Framerate 24
Set TypingSpeed 40ms

# Put the binary on PATH and silence client-go deprecation warnings (stderr
# noise from the deliberately-old demo cluster), then start from a clean screen.
Hide
Type `upgradescope() { ./bin/upgradescope "$@" 2>/dev/null; }; clear`
Enter
Sleep 500ms
Show

Type "upgradescope scan --target 1.36"
Sleep 500ms
Enter
Sleep 4s

Type "kubectl get ucr"
Sleep 500ms
Enter
Sleep 3s
