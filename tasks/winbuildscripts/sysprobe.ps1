$Password = ConvertTo-SecureString "dummyPW_:-gch6Rejae9" -AsPlainText -Force
New-LocalUser -Name "ddagentuser" -Description "Test user for the secrets feature on windows." -Password $Password

$Env:Python2_ROOT_DIR=$Env:TEST_EMBEDDED_PY2
$Env:Python3_ROOT_DIR=$Env:TEST_EMBEDDED_PY3

if ($Env:TARGET_ARCH -eq "x64") {
    & ridk enable
}
& $Env:Python3_ROOT_DIR\python.exe -m  pip install -r requirements.txt

$PROBE_BUILD_ROOT=(Get-Location).Path
$Env:PATH="$PROBE_BUILD_ROOT\dev\lib;$Env:GOPATH\bin;$Env:Python2_ROOT_DIR;$Env:Python2_ROOT_DIR\Scripts;$Env:Python3_ROOT_DIR;$Env:Python3_ROOT_DIR\Scripts;$Env:PATH"

& $Env:Python3_ROOT_DIR\python.exe -m pip install PyYAML==5.3.1
& pip install awscli==1.19.112 python-dateutil==2.8.2 --upgrade

& inv -e deps
& inv -e install-tools

# Must build the rtloader libs cgo depends on before running golangci-lint, which requires code to be compilable
$archflag = "x64"
if ($Env:TARGET_ARCH -eq "x86") {
    $archflag = "x86"
}
& .\tasks\winbuildscripts\pre-go-build.ps1 -Architecture "$archflag" -PythonRuntimes "$Env:PY_RUNTIMES"

& inv -e lint-go --build system-probe-unit-tests --targets .\pkg
$err = $LASTEXITCODE
if ($err -ne 0) {
    Write-Host -ForegroundColor Red "lint-go failed $err"
    [Environment]::Exit($err)
}

& inv -e system-probe.kitchen-prepare --ci

$err = $LASTEXITCODE
Write-Host Test result is $err
if($err -ne 0){
    Write-Host -ForegroundColor Red "kitchen prepare failed $err"
    [Environment]::Exit($err)
}
