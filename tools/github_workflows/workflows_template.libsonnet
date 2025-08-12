{
  local platforms = [
    {
      name: 'linux_amd64',
      buildJustBinaries: false,
      extension: '',
    },
    {
      name: 'linux_386',
      buildJustBinaries: false,
      extension: '',
      testPlatform: 'linux_amd64',
    },
    {
      name: 'linux_arm',
      buildJustBinaries: false,
      extension: '',
    },
    {
      name: 'linux_arm64',
      buildJustBinaries: false,
      extension: '',
    },
    {
      name: 'darwin_amd64',
      buildJustBinaries: false,
      extension: '',
    },
    {
      name: 'darwin_arm64',
      buildJustBinaries: false,
      extension: '',
    },
    {
      name: 'freebsd_amd64',
      buildJustBinaries: false,
      extension: '',
    },
    {
      name: 'windows_amd64',
      buildJustBinaries: false,
      extension: '.exe',
    },
  ],

  local getJobs(binaries, containers, doUpload, enableCgo) = {
    build_and_test: {
      strategy: {
        matrix: {
          host: [
            {
              bazel_startup_flags: '',
              bazel_os: 'linux',
              cross_compile: true,
              lint: true,
              os: 'ubuntu-latest',
              platform_name: 'linux_amd64',
              upload: true,
            },
            {
              // Use the D drive to improve performance.
              bazel_startup_flags: '--output_base=D:/bazel_output',
              bazel_os: 'windows',
              cross_compile: false,
              lint: false,
              os: 'windows-latest',
              platform_name: 'windows_amd64',
              upload: false,
            },
          ],
        },
      },
      'runs-on': '${{ matrix.host.os }}',
      name: 'build_and_test ${{ matrix.host.os }}',
      steps: [
        // TODO: Switch back to l.gcr.io/google/bazel once updated
        // container images get published once again.
        // https://github.com/GoogleCloudPlatform/container-definitions/issues/12037
        {
          name: 'Check out source code',
          uses: 'actions/checkout@v1',
        },
        {
          name: 'Installing Bazel',
          run: 'v=$(cat .bazelversion) && curl -L https://github.com/bazelbuild/bazel/releases/download/${v}/bazel-${v}-${{matrix.host.bazel_os}}-x86_64 > ~/bazel && chmod +x ~/bazel && echo ~ >> ${GITHUB_PATH}',
          shell: 'bash',
        },
        {
          name: 'Bazel mod tidy',
          run: 'bazel ${{ matrix.host.bazel_startup_flags }} mod tidy',
          'if': 'matrix.host.lint',
        },
        {
          name: 'Gazelle',
          run: "rm -f $(find . -name '*.pb.go' | sed -e 's/[^/]*$/BUILD.bazel/') && bazel ${{ matrix.host.bazel_startup_flags }} run //:gazelle",
          'if': 'matrix.host.lint',
        },
        {
          name: 'Buildifier',
          run: 'bazel ${{ matrix.host.bazel_startup_flags }} run @com_github_bazelbuild_buildtools//:buildifier',
          'if': 'matrix.host.lint',
        },
        {
          name: 'Gofmt',
          run: 'bazel ${{ matrix.host.bazel_startup_flags }} run @cc_mvdan_gofumpt//:gofumpt -- -w -extra $(pwd)',
          'if': 'matrix.host.lint',
        },
        {
          name: 'Clang format',
          run: "find . -name '*.proto' -exec bazel ${{ matrix.host.bazel_startup_flags }} run @llvm_toolchain_llvm//:bin/clang-format -- -i {} +",
          'if': 'matrix.host.lint',
        },
        {
          name: 'GitHub workflows',
          run: 'bazel ${{ matrix.host.bazel_startup_flags }} build //tools/github_workflows && cp bazel-bin/tools/github_workflows/*.yaml .github/workflows',
          'if': 'matrix.host.lint',
        },
        {
          name: 'Protobuf generation',
          run: |||
            if [ -d pkg/proto ]; then
              find . bazel-bin/pkg/proto -name '*.pb.go' -delete || true
              bazel ${{ matrix.host.bazel_startup_flags }} build $(bazel query --output=label 'kind("go_proto_library", //...)')
              find bazel-bin/pkg/proto -name '*.pb.go' | while read f; do
                cat $f > $(echo $f | sed -e 's|.*/pkg/proto/|pkg/proto/|')
              done
            fi
          |||,
          'if': 'matrix.host.lint',
        },
        {
          name: 'Embedded asset generation',
          run: |||
            bazel ${{ matrix.host.bazel_startup_flags }} build $(git grep '^[[:space:]]*//go:embed ' | sed -e 's|\(.*\)/.*//go:embed |//\1:|; s|"||g; s| .*||' | sort -u)
            git grep '^[[:space:]]*//go:embed ' | sed -e 's|\(.*\)/.*//go:embed |\1/|' | while read o; do
              if [ -e "bazel-bin/$o" ]; then
                rm -rf "$o"
                cp -r "bazel-bin/$o" "$o"
                find "$o" -type f -exec chmod -x {} +
              fi
            done
          |||,
          'if': 'matrix.host.lint',
        },
        {
          name: 'Test style conformance',
          run: 'git add . && git diff --exit-code HEAD --',
          'if': 'matrix.host.lint',
        },
        {
          name: 'Golint',
          run: 'bazel ${{ matrix.host.bazel_startup_flags }} run @org_golang_x_lint//golint -- -set_exit_status $(pwd)/...',
          'if': 'matrix.host.lint',
        },
      ] + std.flattenArrays([
        [{
          name: platform.name + ': build and test',
          run: ('bazel ${{ matrix.host.bazel_startup_flags }} %s --platforms=@rules_go//go/toolchain:%s ' % [
                  // Run tests only if we're not cross-compiling.
                  "${{ matrix.host.platform_name == '%s' && 'test --test_output=errors' || 'build' }}" % std.get(platform, 'testPlatform', platform.name),
                  platform.name + if enableCgo then '_cgo' else '',
                ]) + (
            if platform.buildJustBinaries
            then std.join(' ', ['//cmd/' + binary for binary in binaries])
            else '//...'
          ),
          'if': "matrix.host.cross_compile || matrix.host.platform_name == '%s'" % platform.name,
        }] + (
          if doUpload
          then std.flattenArrays([
            [
              {
                name: '%s: copy %s' % [platform.name, binary],
                local executable = binary + platform.extension,
                run: 'rm -f %s && bazel ${{ matrix.host.bazel_startup_flags }} run --run_under cp --platforms=@rules_go//go/toolchain:%s //cmd/%s $(pwd)/%s' % [
                  executable,
                  platform.name + if enableCgo then '_cgo' else '',
                  binary,
                  executable,
                ],
                'if': 'matrix.host.upload',
              },
              {
                name: '%s: upload %s' % [platform.name, binary],
                uses: 'actions/upload-artifact@v4',
                with: {
                  name: '%s.%s' % [binary, platform.name],
                  path: binary + platform.extension,
                },
                'if': 'matrix.host.upload',
              },
            ]
            for binary in binaries
          ])
          else []
        )
        for platform in platforms
      ]) + (
        if doUpload
        then (
          [
            {
              name: 'Install Docker credentials',
              run: 'echo "${GITHUB_TOKEN}" | docker login ghcr.io -u $ --password-stdin',
              env: {
                GITHUB_TOKEN: '${{ secrets.GITHUB_TOKEN }}',
              },
              'if': 'matrix.host.upload',
            },
          ] + [
            {
              name: 'Push container %s' % container,
              run: 'bazel ${{ matrix.host.bazel_startup_flags }} run --stamp //cmd/%s_container_push' % container,
              'if': 'matrix.host.upload',
            }
            for container in containers
          ]
        )
        else []
      ),
    },
  },

  getWorkflows(binaries, containers): {
    'master.yaml': {
      name: 'master',
      on: { push: { branches: ['main', 'master'] } },
      jobs: getJobs(binaries, containers, true, false),
    },
    'pull-requests.yaml': {
      name: 'pull-requests',
      on: { pull_request: { branches: ['main', 'master'] } },
      jobs: getJobs(binaries, containers, false, false),
    },
  },
}
