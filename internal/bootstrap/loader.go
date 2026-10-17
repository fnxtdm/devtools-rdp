package bootstrap

import (
	"bytes"
	"compress/gzip"
	"encoding/base64"
	"fmt"
)

// loaderCS is compiled in the RDP session with Add-Type. It opens the RDP
// static virtual channel, signals READY, receives the agent config and
// executable, then starts the native agent.
const loaderCS = `using System;using System.IO;using System.Text;using System.Threading;using System.Diagnostics;using System.Runtime.InteropServices;
public static class DevToolsRdpLoader{
[DllImport("wtsapi32.dll",CharSet=CharSet.Ansi)]static extern IntPtr WTSVirtualChannelOpen(IntPtr h,int s,string n);
[DllImport("wtsapi32.dll")]static extern bool WTSVirtualChannelRead(IntPtr c,uint t,byte[] b,int l,out int r);
[DllImport("wtsapi32.dll")]static extern bool WTSVirtualChannelWrite(IntPtr c,byte[] b,int l,out int w);
[DllImport("wtsapi32.dll")]static extern bool WTSVirtualChannelClose(IntPtr c);
static byte[] B=new byte[65536];static int N=0,P=0;
public static int Run(string name,string dir){
IntPtr c=WTSVirtualChannelOpen(IntPtr.Zero,-1,name);if(c==IntPtr.Zero)return 2;
int z=0;byte[] R=Encoding.ASCII.GetBytes("READY");WTSVirtualChannelWrite(c,R,R.Length,out z);
byte[] cfg=Blob(c);if(cfg==null)return 3;byte[] exe=Blob(c);if(exe==null)return 4;
string cp=Path.Combine(dir,"devtools-rdp-agent.json");string ep=Path.Combine(dir,"devtools-rdp-agent-"+DateTime.UtcNow.Ticks+".exe");
File.WriteAllBytes(cp,cfg);File.WriteAllBytes(ep,exe);
WTSVirtualChannelClose(c);
ProcessStartInfo psi=new ProcessStartInfo(ep,"--config \""+cp+"\"");psi.UseShellExecute=false;psi.CreateNoWindow=true;
Process.Start(psi);return 0;}
static byte[] Blob(IntPtr c){byte[] h=new byte[4];if(!RE(c,h,4))return null;int n=BitConverter.ToInt32(h,0);
if(n<=0||n>67108864)return null;byte[] d=new byte[n];if(!RE(c,d,n))return null;return d;}
static bool RE(IntPtr c,byte[] d,int n){int g=0;while(g<n){if(P>=N){int r;
if(!WTSVirtualChannelRead(c,uint.MaxValue,B,B.Length,out r)){int e=Marshal.GetLastWin32Error();
if(e==232||e==996||e==997){Thread.Sleep(5);continue;}return false;}if(r<=0)continue;N=r;P=0;}
int k=Math.Min(N-P,n-g);Array.Copy(B,P,d,g,k);P+=k;g+=k;}return true;}}`

// LoaderCommand returns the PowerShell line typed into the remote console.
// The payload is gzip+base64 so the keystroke sequence stays short.
func LoaderCommand(channel string) (string, error) {
	script := fmt.Sprintf("Add-Type -TypeDefinition @'\n%s\n'@\n[DevToolsRdpLoader]::Run('%s', $env:TEMP)\n", loaderCS, channel)
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write([]byte(script)); err != nil {
		return "", err
	}
	if err := zw.Close(); err != nil {
		return "", err
	}
	encoded := base64.StdEncoding.EncodeToString(buf.Bytes())
	return "$m=New-Object IO.MemoryStream(,[Convert]::FromBase64String('" + encoded +
		"'));iex((New-Object IO.StreamReader((New-Object IO.Compression.GZipStream($m,[IO.Compression.CompressionMode]::Decompress)))).ReadToEnd())", nil
}
