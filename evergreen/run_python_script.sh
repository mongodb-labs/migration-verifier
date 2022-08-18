unset workdir
DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" > /dev/null 2>&1 && pwd)"
. "$DIR/prelude.sh"

set -o errexit
set -o verbose

eval "${PREPARE_SHELL}"
PATH=$PATH:$HOME

activate_venv

$python $@
