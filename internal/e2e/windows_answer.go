//go:build windows

package e2e

import (
	"fmt"
	"strings"
)

const (
	// winVMName doubles as the guest's computer name, so it has to stay within
	// the 15-character NetBIOS limit or Setup rejects the answer file.
	winVMName    = "win2022-test"
	winISO       = `D:\HyperV\ISO\WinServer2022-eval.iso`
	winAnswerISO = `D:\HyperV\ISO\win2022-test-answer.iso`
	winVMPath    = `D:\HyperV\VMs`
	winVHD       = `D:\HyperV\VHD\win2022-test.vhdx`
	winAdmin     = "Administrator"
	// A fixed password for a disposable test VM that exists only to be rebuilt,
	// deliberately in the open so a run is reproducible. It satisfies the default
	// Windows complexity policy, which Setup enforces even unattended.
	winPassword = "Hyp3rVM-e2e!"
	winSwitch   = "Default Switch"
)

// autounattendXML is the answer file that installs Windows Server unattended.
//
// Windows Setup looks for this by name at the root of removable media, which is
// why it travels on an ISO rather than on the seed disk Anaconda uses: a virtual
// hard disk is not removable, so Setup never looks at one.
//
// Two things here are load-bearing for the tests that follow.
//
// The image is chosen by name. On the evaluation media "Windows Server 2022
// SERVERSTANDARD" is the Desktop Experience edition; the Server Core one is
// SERVERSTANDARDCORE. The guest therefore has a full desktop, which matters for
// anything GUI: PowerShell Direct lands in session 0, so a window can only be
// drawn or captured in the interactive session this edition provides.
//
// The administrator password is set and auto-logon is enabled once, because
// PowerShell Direct authenticates with a password and there is no other way in
// before the network exists.
func autounattendXML(admin, password string) string {
	return `<?xml version="1.0" encoding="utf-8"?>
<unattend xmlns="urn:schemas-microsoft-com:unattend">

  <settings pass="windowsPE">
    <component name="Microsoft-Windows-International-Core-WinPE"
               processorArchitecture="amd64"
               publicKeyToken="31bf3856ad364e35" language="neutral" versionScope="nonSxS">
      <SetupUILanguage><UILanguage>en-US</UILanguage></SetupUILanguage>
      <InputLocale>en-US</InputLocale>
      <SystemLocale>en-US</SystemLocale>
      <UILanguage>en-US</UILanguage>
      <UserLocale>en-US</UserLocale>
    </component>

    <component name="Microsoft-Windows-Setup"
               processorArchitecture="amd64"
               publicKeyToken="31bf3856ad364e35" language="neutral" versionScope="nonSxS">
      <DiskConfiguration>
        <!-- Disk 0 is the boot VHDX. WillWipeDisk makes this idempotent across
             reinstalls onto the same disk. -->
        <Disk wcm:action="add" xmlns:wcm="http://schemas.microsoft.com/WMIConfig/2002/State">
          <DiskID>0</DiskID>
          <WillWipeDisk>true</WillWipeDisk>
          <CreatePartitions>
            <!-- Generation 2 is UEFI, so this needs the full GPT layout:
                 EFI system partition, MSR, then Windows. -->
            <CreatePartition wcm:action="add">
              <Order>1</Order><Type>EFI</Type><Size>260</Size>
            </CreatePartition>
            <CreatePartition wcm:action="add">
              <Order>2</Order><Type>MSR</Type><Size>128</Size>
            </CreatePartition>
            <CreatePartition wcm:action="add">
              <Order>3</Order><Type>Primary</Type><Extend>true</Extend>
            </CreatePartition>
          </CreatePartitions>
          <ModifyPartitions>
            <ModifyPartition wcm:action="add">
              <Order>1</Order><PartitionID>1</PartitionID>
              <Format>FAT32</Format><Label>System</Label>
            </ModifyPartition>
            <ModifyPartition wcm:action="add">
              <Order>2</Order><PartitionID>3</PartitionID>
              <Format>NTFS</Format><Label>Windows</Label><Letter>C</Letter>
            </ModifyPartition>
          </ModifyPartitions>
        </Disk>
      </DiskConfiguration>

      <ImageInstall>
        <OSImage>
          <InstallFrom>
            <MetaData wcm:action="add" xmlns:wcm="http://schemas.microsoft.com/WMIConfig/2002/State">
              <Key>/IMAGE/NAME</Key>
              <Value>Windows Server 2022 SERVERSTANDARD</Value>
            </MetaData>
          </InstallFrom>
          <InstallTo><DiskID>0</DiskID><PartitionID>3</PartitionID></InstallTo>
        </OSImage>
      </ImageInstall>

      <UserData>
        <AcceptEula>true</AcceptEula>
        <FullName>hypervm-mcp</FullName>
        <Organization>hypervm-mcp</Organization>
      </UserData>
    </component>
  </settings>

  <settings pass="specialize">
    <component name="Microsoft-Windows-Shell-Setup"
               processorArchitecture="amd64"
               publicKeyToken="31bf3856ad364e35" language="neutral" versionScope="nonSxS">
      <ComputerName>` + winVMName + `</ComputerName>
      <TimeZone>UTC</TimeZone>
    </component>
    <component name="Microsoft-Windows-TerminalServices-LocalSessionManager"
               processorArchitecture="amd64"
               publicKeyToken="31bf3856ad364e35" language="neutral" versionScope="nonSxS">
      <fDenyTSConnections>false</fDenyTSConnections>
    </component>
  </settings>

  <settings pass="oobeSystem">
    <component name="Microsoft-Windows-Shell-Setup"
               processorArchitecture="amd64"
               publicKeyToken="31bf3856ad364e35" language="neutral" versionScope="nonSxS">
      <OOBE>
        <HideEULAPage>true</HideEULAPage>
        <HideLocalAccountScreen>true</HideLocalAccountScreen>
        <HideOnlineAccountScreens>true</HideOnlineAccountScreens>
        <HideWirelessSetupInOOBE>true</HideWirelessSetupInOOBE>
        <ProtectYourPC>3</ProtectYourPC>
        <NetworkLocation>Work</NetworkLocation>
      </OOBE>

      <UserAccounts>
        <AdministratorPassword>
          <Value>` + xmlEscape(password) + `</Value>
          <PlainText>true</PlainText>
        </AdministratorPassword>
      </UserAccounts>

      <!-- Automatic logon, and not just once.
           PowerShell Direct needs the account to have signed in at least one
           time, which a single logon would satisfy. The high count is for
           something else: anything graphical has to run in an interactive
           session, and one only exists while somebody is logged on. With
           LogonCount 1 the desktop would vanish at the first reboot and every
           GUI test after it would fail with nowhere to draw. -->
      <AutoLogon>
        <Enabled>true</Enabled>
        <LogonCount>1000</LogonCount>
        <Username>` + xmlEscape(admin) + `</Username>
        <Password>
          <Value>` + xmlEscape(password) + `</Value>
          <PlainText>true</PlainText>
        </Password>
      </AutoLogon>

      <FirstLogonCommands>
        <!-- A marker the tests wait for. Setup reports "finished" long before
             first logon completes, and PowerShell Direct answers somewhere in
             between, so the tests need a signal that means genuinely ready. -->
        <SynchronousCommand wcm:action="add" xmlns:wcm="http://schemas.microsoft.com/WMIConfig/2002/State">
          <Order>1</Order>
          <CommandLine>cmd /c echo ready &gt; C:\hypervm-mcp-ready.txt</CommandLine>
          <Description>Signal readiness</Description>
        </SynchronousCommand>
        <!-- A locked or blanked screen captures as black and takes no clicks,
             which reads as a broken test rather than a sleeping desktop. -->
        <SynchronousCommand wcm:action="add" xmlns:wcm="http://schemas.microsoft.com/WMIConfig/2002/State">
          <Order>2</Order>
          <CommandLine>reg add "HKCU\Control Panel\Desktop" /v ScreenSaveActive /t REG_SZ /d 0 /f</CommandLine>
          <Description>Never blank the screen</Description>
        </SynchronousCommand>
        <SynchronousCommand wcm:action="add" xmlns:wcm="http://schemas.microsoft.com/WMIConfig/2002/State">
          <Order>3</Order>
          <CommandLine>reg add "HKLM\SOFTWARE\Microsoft\Windows\CurrentVersion\Policies\System" /v DisableLockWorkstation /t REG_DWORD /d 1 /f</CommandLine>
          <Description>Never lock the workstation</Description>
        </SynchronousCommand>
        <SynchronousCommand wcm:action="add" xmlns:wcm="http://schemas.microsoft.com/WMIConfig/2002/State">
          <Order>4</Order>
          <CommandLine>powercfg /change monitor-timeout-ac 0</CommandLine>
          <Description>Never turn the display off</Description>
        </SynchronousCommand>
        <!-- Deliberately NOT installing OpenSSH here. Bootstrapping sshd through
             PowerShell Direct, over the VMBus and with no guest network, is one
             of the things these tests exist to prove. -->
      </FirstLogonCommands>
    </component>
  </settings>
</unattend>
`
}

// xmlEscape keeps a password with markup characters from breaking the answer
// file, which would otherwise fail at install time with nothing to look at.
func xmlEscape(s string) string {
	r := strings.NewReplacer(
		"&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;", "'", "&apos;")
	return r.Replace(s)
}

// buildAnswerISOScript renders the PowerShell that writes an answer file to an
// ISO using IMAPI2.
//
// This lives in the test harness rather than in the server on purpose: putting
// media in front of a VM is the server's job, authoring it is not. IMAPI2 ships
// with Windows, so the harness needs no ADK and no oscdimg.
func buildAnswerISOScript(isoPath, xml string) string {
	return fmt.Sprintf(`
$ErrorActionPreference = 'Stop'
$iso   = %q
$stage = Join-Path $env:TEMP ('winanswer-' + [guid]::NewGuid().ToString('N'))

New-Item -ItemType Directory -Path $stage -Force | Out-Null
try {
    # Windows Setup reads this as UTF-8; a BOM is tolerated but not needed.
    [System.IO.File]::WriteAllText(
        (Join-Path $stage 'autounattend.xml'), $env:HYPERVM_ANSWER,
        (New-Object System.Text.UTF8Encoding($false)))

    $fsi = New-Object -ComObject IMAPI2FS.MsftFileSystemImage
    # ISO9660 plus Joliet: Setup looks for exactly "autounattend.xml", which the
    # short-name filesystem alone would not preserve.
    $fsi.FileSystemsToCreate = 3
    $fsi.VolumeName = 'UNATTEND'
    $fsi.Root.AddTree($stage, $false)
    $img = $fsi.CreateResultImage()

    if (-not ('HyperVMIsoWriter' -as [type])) {
        Add-Type -TypeDefinition @'
using System;
using System.IO;
using System.Runtime.InteropServices;
using System.Runtime.InteropServices.ComTypes;

public static class HyperVMIsoWriter
{
    public static void Write(object stream, string path, int blockSize, int totalBlocks)
    {
        IStream source = (IStream)stream;
        IntPtr read = Marshal.AllocHGlobal(4);
        try
        {
            byte[] buffer = new byte[blockSize];
            using (FileStream output = File.Open(path, FileMode.Create, FileAccess.Write))
            {
                while (totalBlocks-- > 0)
                {
                    source.Read(buffer, blockSize, read);
                    output.Write(buffer, 0, Marshal.ReadInt32(read));
                }
                output.Flush();
            }
        }
        finally { Marshal.FreeHGlobal(read); }
    }
}
'@
    }
    [HyperVMIsoWriter]::Write($img.ImageStream, $iso, $img.BlockSize, $img.TotalBlocks)
} finally {
    Remove-Item -LiteralPath $stage -Recurse -Force -ErrorAction SilentlyContinue
}
Write-Output ((Get-Item -LiteralPath $iso).Length)
`, isoPath)
}
