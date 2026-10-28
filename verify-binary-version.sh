#!/bin/sh
# 校验「要装进镜像的那个二进制」就是本次构建刚编出来的那一份。
#
# 用法：verify-binary-version.sh <二进制路径> <期望版本>
#
# 为什么必须跑一次二进制、而不是 grep 二进制里的版本串：版本号常常只有七位十六
# 进制（如 2551f60），在几十 MB 的二进制里偶然出现同一串的概率并不低；版本号本身
# 也可能短到无从区分。跑一次让它自己报，才拿得到准确答案。
#
# 退出码：0=一致；1=版本不符；2=用法/环境问题（校验没做成，同样算失败）。
set -eu

if [ "$#" -ne 2 ]; then
	echo "用法：$0 <二进制路径> <期望版本>" >&2
	exit 2
fi
bin="$1"
expected="$2"

if [ -z "$expected" ]; then
	echo "缺少期望版本：构建镜像时必须用 --build-arg ARTEX_BINARY_VERSION=<版本> 传入，" >&2
	echo "否则无法确认装进镜像的二进制就是刚编出来的那份。请用 ./build-image.sh 构建。" >&2
	exit 2
fi

if [ ! -x "$bin" ]; then
	echo "二进制不存在或不可执行：$bin" >&2
	exit 2
fi

actual="$("$bin" -version 2>/dev/null || true)"
actual="$(printf '%s' "$actual" | tr -d '\r\n')"
if [ -z "$actual" ]; then
	echo "读不到二进制版本（跑不起来或没有输出）：$bin" >&2
	exit 2
fi

# 版本号前的 v 只是 tag 习惯，两边都去掉再比。
want="${expected#v}"
got="${actual#v}"
if [ "$got" != "$want" ]; then
	echo "镜像里的二进制版本与期望不符：期望 $want，实际 $got" >&2
	echo "多半是 $bin 是上一次构建留下的旧文件（构建产物目录不会自动清空）。" >&2
	echo "重新跑一次 ./build-image.sh 即可。" >&2
	exit 1
fi

exit 0
