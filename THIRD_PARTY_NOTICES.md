# 第三方组件声明

本项目的自有代码以 MIT 许可发布，见 [LICENSE](LICENSE)。

下面列出的是本项目**依赖并会编译进产物**的第三方组件。按各组件自己的许可要求，
这里逐字保留其原始版权声明与许可全文。

---

## rsc.io/qr

- 版本：v0.2.0
- 来源：https://github.com/rsc/qr
- 许可：BSD 3-Clause
- 用途：在服务端把内容编码成二维码 PNG，前端因此不需要任何二维码 JS 库
- 使用位置：webui.go

```text
Copyright (c) 2009 The Go Authors. All rights reserved.

Redistribution and use in source and binary forms, with or without
modification, are permitted provided that the following conditions are
met:

   * Redistributions of source code must retain the above copyright
notice, this list of conditions and the following disclaimer.
   * Redistributions in binary form must reproduce the above
copyright notice, this list of conditions and the following disclaimer
in the documentation and/or other materials provided with the
distribution.
   * Neither the name of Google Inc. nor the names of its
contributors may be used to endorse or promote products derived from
this software without specific prior written permission.

THIS SOFTWARE IS PROVIDED BY THE COPYRIGHT HOLDERS AND CONTRIBUTORS
"AS IS" AND ANY EXPRESS OR IMPLIED WARRANTIES, INCLUDING, BUT NOT
LIMITED TO, THE IMPLIED WARRANTIES OF MERCHANTABILITY AND FITNESS FOR
A PARTICULAR PURPOSE ARE DISCLAIMED. IN NO EVENT SHALL THE COPYRIGHT
OWNER OR CONTRIBUTORS BE LIABLE FOR ANY DIRECT, INDIRECT, INCIDENTAL,
SPECIAL, EXEMPLARY, OR CONSEQUENTIAL DAMAGES (INCLUDING, BUT NOT
LIMITED TO, PROCUREMENT OF SUBSTITUTE GOODS OR SERVICES; LOSS OF USE,
DATA, OR PROFITS; OR BUSINESS INTERRUPTION) HOWEVER CAUSED AND ON ANY
THEORY OF LIABILITY, WHETHER IN CONTRACT, STRICT LIABILITY, OR TORT
(INCLUDING NEGLIGENCE OR OTHERWISE) ARISING IN ANY WAY OUT OF THE USE
OF THIS SOFTWARE, EVEN IF ADVISED OF THE POSSIBILITY OF SUCH DAMAGE.
```

---

## 为什么仓库里需要这个文件

BSD 3-Clause 的前两条是硬性要求，不是「最好这样」：

1. 以**源码**形式再分发时，必须保留版权声明、这份条件列表和免责声明；
2. 以**二进制**形式再分发时，必须在随附的文档或其他材料中重现版权声明、条件
   列表和免责声明。

在 GitHub 上发布源码命中第 1 条；发布编译好的可执行文件或 Docker 镜像命中第 2 条。
所以本文件要跟源码一起提交，**发布镜像时也要一并带上** —— `Dockerfile` 里已经把
它复制进运行镜像了。

第 3 条是限制：不得用 Google Inc. 或其贡献者的名义为本项目背书或推广。

---

## 如果以后新增了依赖

`go.mod` 里每多一个 `require`，就照上面的格式在这里补一段，**不要改写成中文，
也不要省略免责声明那一段**。查许可最简单的方式：

```bash
go list -m all                                  # 看有哪些模块
go mod download -x                              # 把源码下到模块缓存
# 许可文件就在模块缓存里对应的模块目录下，通常叫 LICENSE 或 COPYING
```

反过来，像 WeCom-Push-API 那样 `go.mod` 里一条 `require` 都没有的项目，仓库里
就不需要这个文件 —— 也就不会有这个问题。
